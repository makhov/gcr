//go:build linux

// Package setup installs gcr on a node and checks the node for what gcr's
// features need (gcr setup, gcr doctor).
package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/makhov/gcr/internal/payload"
)

const (
	// DataDir holds gcr's state on a node, and the CRIU gcr setup installs.
	DataDir = "/var/lib/gcr"
	// RuntimeType is gcr's containerd runtime type.
	RuntimeType = "io.containerd.gcr.v2"
)

var (
	// CRIUDir and PluginDir hold the CRIU setup installs, used rather than
	// any other criu on the node (UseOwnCRIU).
	CRIUDir   = filepath.Join(DataDir, "bin")
	PluginDir = filepath.Join(DataDir, "lib", "criu")
)

// UseOwnCRIU makes the criu gcr setup installed the one this process and
// what it starts run, with its plugins: libcontainer, and gcr's own calls,
// run criu from PATH. A node without one keeps its own criu.
func UseOwnCRIU() {
	if _, err := os.Stat(filepath.Join(CRIUDir, "criu")); err != nil {
		return
	}
	os.Setenv("PATH", CRIUDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := os.Stat(PluginDir); err == nil {
		os.Setenv("CRIU_LIBS_DIR", PluginDir)
	}
}

// Options say what Run sets up.
type Options struct {
	// BinDir gets gcr and the shim (default /usr/local/bin).
	BinDir string
	// ContainerdConfig is containerd's configuration (default
	// /etc/containerd/config.toml); gcr's goes in conf.d beside it.
	ContainerdConfig string
	// Docker registers gcr with dockerd too, as runtime "gcr".
	Docker bool
	// NoRestart leaves containerd (and dockerd) running as they are: the
	// configuration takes effect on their next start.
	NoRestart bool
	Out       io.Writer
}

// Run installs what this gcr carries (payload) and registers the runtime
// with containerd.
func Run(o Options) error {
	if o.BinDir == "" {
		o.BinDir = "/usr/local/bin"
	}
	if o.ContainerdConfig == "" {
		o.ContainerdConfig = "/etc/containerd/config.toml"
	}
	say := func(format string, args ...any) { fmt.Fprintf(o.Out, format+"\n", args...) }

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if dst := filepath.Join(o.BinDir, "gcr"); self != dst {
		if err := copyFile(self, dst, 0o755); err != nil {
			return fmt.Errorf("install gcr: %w", err)
		}
		say("installed %s", dst)
	}
	shim := filepath.Join(o.BinDir, payload.Shim)
	switch {
	case payload.Has(payload.Shim):
		if err := payload.Install(payload.Shim, shim, 0o755); err != nil {
			return fmt.Errorf("install the shim: %w", err)
		}
		say("installed %s", shim)
	default:
		// A build without a payload (from source): the shim is installed
		// beside it, or must be.
		if _, err := exec.LookPath(payload.Shim); err != nil {
			return fmt.Errorf("no %s in PATH, and this gcr does not carry one (a release build does): "+
				"install it first (make install)", payload.Shim)
		}
		say("this gcr carries no shim or CRIU: using the installed %s", payload.Shim)
	}
	if payload.Has(payload.CRIU) {
		if err := payload.Install(payload.CRIU, filepath.Join(CRIUDir, "criu"), 0o755); err != nil {
			return fmt.Errorf("install CRIU: %w", err)
		}
		say("installed %s", filepath.Join(CRIUDir, "criu"))
		if payload.Has(payload.CUDAPlugin) {
			if err := payload.Install(payload.CUDAPlugin, filepath.Join(PluginDir, payload.CUDAPlugin), 0o755); err != nil {
				return fmt.Errorf("install CRIU's CUDA plugin: %w", err)
			}
			say("installed %s", filepath.Join(PluginDir, payload.CUDAPlugin))
		}
	}

	changed, err := registerContainerd(o.ContainerdConfig, containerdMajor(), say)
	if err != nil {
		return fmt.Errorf("containerd: %w", err)
	}
	if changed && !o.NoRestart {
		if err := restart("containerd"); err != nil {
			return err
		}
		say("restarted containerd (running containers carry on)")
	}
	if o.Docker {
		changed, err := registerDocker("/etc/docker/daemon.json", say)
		if err != nil {
			return fmt.Errorf("docker: %w", err)
		}
		if changed && !o.NoRestart {
			if err := restart("docker"); err != nil {
				return err
			}
			say("restarted docker")
		}
	}
	return nil
}

// containerdDropIn registers gcr with containerd's CRI plugin, in the
// configuration format containerd v<major> reads; ctr and other clients
// name the runtime type and need nothing.
func containerdDropIn(major int) string {
	table := `[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]`
	version := 3
	if major < 2 {
		table = `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.gcr]`
		version = 2
	}
	return fmt.Sprintf(`# Written by gcr setup.
version = %d
%s
  runtime_type = '%s'
  # gcr's pod annotations; the handler's PodAnnotations says which a pod
  # may use (docs/configuration.md).
  pod_annotations = ['gcr.dev/*']
`, version, table, RuntimeType)
}

// containerdMajor is the installed containerd's major version, 2 if it
// cannot tell.
func containerdMajor() int {
	out, err := exec.Command("containerd", "--version").Output()
	if err != nil {
		return 2
	}
	m := regexp.MustCompile(` v(\d+)\.`).FindSubmatch(out)
	if m == nil || string(m[1]) != "1" {
		return 2
	}
	return 1
}

var importsLine = regexp.MustCompile(`(?m)^\s*imports\s*=\s*\[(.*)\]`)

// registerContainerd writes gcr's drop-in into conf.d beside config, and
// makes config import conf.d if it does not: containerd imports it by
// default, but not once there is a config file without it. It says
// whether anything changed.
func registerContainerd(config string, major int, say func(string, ...any)) (bool, error) {
	confd := filepath.Join(filepath.Dir(config), "conf.d")
	pattern := filepath.Join(confd, "*.toml")
	changed, err := writeIfChanged(filepath.Join(confd, "gcr.toml"), []byte(containerdDropIn(major)), 0o644)
	if err != nil {
		return false, err
	}
	if changed {
		say("wrote %s", filepath.Join(confd, "gcr.toml"))
	}
	b, err := os.ReadFile(config)
	switch {
	case errors.Is(err, os.ErrNotExist) && major >= 2:
		// containerd 2's defaults import conf.d.
		return changed, nil
	case errors.Is(err, os.ErrNotExist):
		b := fmt.Sprintf("# Written by gcr setup: containerd 1 imports nothing by default.\nversion = 2\nimports = ['%s']\n", pattern)
		if err := os.WriteFile(config, []byte(b), 0o644); err != nil {
			return false, err
		}
		say("wrote %s, importing %s", config, pattern)
		return true, nil
	case err != nil:
		return false, err
	}
	if m := importsLine.FindSubmatch(b); m != nil {
		if bytes.Contains(m[1], []byte(pattern)) {
			return changed, nil
		}
		// Add conf.d to the imports there are.
		list := strings.TrimSpace(string(m[1]))
		if list != "" {
			list += ", "
		}
		b = importsLine.ReplaceAll(b, []byte(fmt.Sprintf("imports = [%s'%s']", list, pattern)))
	} else {
		// Top-level keys go before the first table.
		line := []byte(fmt.Sprintf("imports = ['%s']\n", pattern))
		if i := bytes.Index(b, []byte("\n[")); i >= 0 {
			b = append(b[:i+1:i+1], append(line, b[i+1:]...)...)
		} else {
			b = append(b, line...)
		}
	}
	if err := os.WriteFile(config+".gcr-backup", mustRead(config), 0o600); err != nil {
		return false, err
	}
	if err := os.WriteFile(config, b, 0o644); err != nil {
		return false, err
	}
	say("%s imports %s now (the old one is %s.gcr-backup)", config, pattern, config)
	return true, nil
}

// registerDocker adds runtime "gcr" to dockerd's daemon.json.
func registerDocker(path string, say func(string, ...any)) (bool, error) {
	cfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	runtimes, _ := cfg["runtimes"].(map[string]any)
	if runtimes == nil {
		runtimes = map[string]any{}
	}
	if r, ok := runtimes["gcr"].(map[string]any); ok && r["runtimeType"] == RuntimeType {
		return false, nil
	}
	runtimes["gcr"] = map[string]any{"runtimeType": RuntimeType}
	cfg["runtimes"] = runtimes
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".gcr-backup", old, 0o600); err != nil {
			return false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return false, err
	}
	say("registered runtime gcr in %s", path)
	return true, nil
}

func restart(unit string) error {
	if out, err := exec.Command("systemctl", "restart", unit).CombinedOutput(); err != nil {
		return fmt.Errorf("restart %s: %w: %s (restart it yourself, or rerun with -no-restart)", unit, err, bytes.TrimSpace(out))
	}
	return nil
}

func writeIfChanged(path string, b []byte, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, b, mode)
}

func mustRead(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".gcr-new"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
