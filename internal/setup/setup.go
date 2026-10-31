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
	"strconv"
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
	// ContainerdUnit is the systemd unit running containerd (default
	// containerd; k3s and RKE2 run their own).
	ContainerdUnit string
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
	if o.ContainerdUnit == "" {
		o.ContainerdUnit = "containerd"
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
		path, err := exec.LookPath(payload.Shim)
		if err != nil {
			return fmt.Errorf("no %s in PATH, and this gcr does not carry one (a release build does): "+
				"install it first (make install)", payload.Shim)
		}
		if shim, err = filepath.Abs(path); err != nil {
			return err
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

	if _, err := os.Stat(k0sConfig); err == nil {
		if _, err := registerK0s(shim, say); err != nil {
			return fmt.Errorf("k0s: %w", err)
		}
	} else if d := detectDistro(); d != nil {
		if err := registerDistro(d, shim, o.NoRestart, say); err != nil {
			return fmt.Errorf("%s: %w", d.Name, err)
		}
	} else if err := setupContainerd(o, shim, say); err != nil {
		return fmt.Errorf("containerd: %w", err)
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

// setupContainerd registers gcr with containerd and restarts it, having
// checked containerd loads the new configuration; if it would not, or
// does not start with it, the configuration is put back as it was.
func setupContainerd(o Options, shim string, say func(string, ...any)) error {
	changed, undo, err := registerContainerd(o.ContainerdConfig, shim, containerdMajor(), say)
	if err != nil {
		if undo != nil {
			undo()
		}
		return err
	}
	if !changed {
		return nil
	}
	if err := validate(o.ContainerdConfig); err != nil {
		undo()
		return fmt.Errorf("%w; left the configuration as it was", err)
	}
	if o.NoRestart {
		return nil
	}
	if err := restart(o.ContainerdUnit); err != nil {
		undo()
		if rerr := restart(o.ContainerdUnit); rerr != nil {
			return fmt.Errorf("%w; put the configuration back, and restarting %s with it failed too: %v", err, o.ContainerdUnit, rerr)
		}
		return fmt.Errorf("%w; put the configuration back and restarted %s with it", err, o.ContainerdUnit)
	}
	say("restarted %s (running containers carry on)", o.ContainerdUnit)
	return nil
}

// containerdDropIn registers gcr with containerd's CRI plugin, in
// containerd's configuration format version (2, or 3 and later: a drop-in
// may not be newer than the configuration importing it); ctr and other
// clients name the runtime type and need nothing.
//
// It names the shim by its path: containerd would look for it on its own
// PATH, which k0s's, for one, sets for itself.
func containerdDropIn(version int, shim string) string {
	table := `[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]`
	if version < 3 {
		table = `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.gcr]`
	} else {
		version = 3
	}
	return fmt.Sprintf(`# Written by gcr setup.
version = %d
%s
  runtime_type = '%s'
  runtime_path = '%s'
  # gcr's pod annotations; the handler's PodAnnotations says which a pod
  # may use (docs/configuration.md).
  pod_annotations = ['gcr.dev/*']
`, version, table, RuntimeType, shim)
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

var (
	importsLine = regexp.MustCompile(`(?m)^\s*imports\s*=\s*\[(.*)\]`)
	versionLine = regexp.MustCompile(`(?m)^\s*version\s*=\s*(\d+)`)
)

// rootVersion is the format version of containerd's configuration b, or
// of its defaults (b nil) for containerd v<major>. Without a version line
// a configuration is version 1.
func rootVersion(b []byte, major int) int {
	if b == nil {
		if major < 2 {
			return 2
		}
		return 3
	}
	m := versionLine.FindSubmatch(b)
	if m == nil {
		return 1
	}
	v, _ := strconv.Atoi(string(m[1]))
	return v
}

// registerContainerd writes gcr's drop-in into conf.d beside config, and
// makes config import conf.d if it does not: containerd imports it by
// default, but not once there is a config file without it. It says
// whether anything changed, and undo puts things back as they were.
func registerContainerd(config, shim string, major int, say func(string, ...any)) (changed bool, undo func(), err error) {
	confd := filepath.Join(filepath.Dir(config), "conf.d")
	dropIn := filepath.Join(confd, "gcr.toml")
	pattern := filepath.Join(confd, "*.toml")
	b, err := os.ReadFile(config)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, nil, err
	}
	version := rootVersion(b, major)
	if version < 2 {
		return false, nil, fmt.Errorf("%s is in configuration format version 1, which imports nothing: migrate it (containerd config migrate)", config)
	}

	oldDropIn, dropInErr := os.ReadFile(dropIn)
	undos := []func(){func() {
		if dropInErr == nil {
			_ = os.WriteFile(dropIn, oldDropIn, 0o644)
		} else {
			_ = os.Remove(dropIn)
		}
	}}
	undo = func() {
		for i := len(undos) - 1; i >= 0; i-- {
			undos[i]()
		}
	}
	changed, err = writeIfChanged(dropIn, []byte(containerdDropIn(version, shim)), 0o644)
	if err != nil {
		return false, undo, err
	}
	if changed {
		say("wrote %s", dropIn)
	}

	var next []byte
	switch {
	case b == nil && major >= 2:
		// containerd 2's defaults import conf.d.
		return changed, undo, nil
	case b == nil:
		next = []byte(fmt.Sprintf("# Written by gcr setup: containerd 1 imports nothing by default.\nversion = 2\nimports = ['%s']\n", pattern))
	default:
		if m := importsLine.FindSubmatch(b); m != nil {
			if bytes.Contains(m[1], []byte(pattern)) {
				return changed, undo, nil
			}
			// Add conf.d to the imports there are.
			list := strings.TrimSpace(string(m[1]))
			if list != "" {
				list += ", "
			}
			next = importsLine.ReplaceAll(b, []byte(fmt.Sprintf("imports = [%s'%s']", list, pattern)))
		} else {
			// Top-level keys go before the first table.
			line := []byte(fmt.Sprintf("imports = ['%s']\n", pattern))
			if i := bytes.Index(b, []byte("\n[")); i >= 0 {
				next = append(append(append([]byte{}, b[:i+1]...), line...), b[i+1:]...)
			} else {
				next = append(append([]byte{}, b...), line...)
			}
		}
		if err := os.WriteFile(config+".gcr-backup", b, 0o600); err != nil {
			return changed, undo, err
		}
	}
	undos = append(undos, func() {
		if b == nil {
			_ = os.Remove(config)
		} else {
			_ = os.WriteFile(config, b, 0o644)
		}
	})
	if err := os.WriteFile(config, next, 0o644); err != nil {
		return changed, undo, err
	}
	if b == nil {
		say("wrote %s, importing %s", config, pattern)
	} else {
		say("%s imports %s now (the old one is %s.gcr-backup)", config, pattern, config)
	}
	return true, undo, nil
}

// validate has containerd load config and what it imports (config dump),
// which catches what would not parse. It is not all a start checks (a
// drop-in newer than the configuration passes it): a failed restart puts
// the configuration back too (setupContainerd).
func validate(config string) error {
	if _, err := exec.LookPath("containerd"); err != nil {
		return nil // not on PATH (k0s keeps its own); its start will say
	}
	out, err := exec.Command("containerd", "--config", config, "config", "dump").CombinedOutput()
	if err != nil {
		return fmt.Errorf("containerd would not load the configuration: %s", lastLine(string(out)))
	}
	return nil
}

// k0sConfig is the containerd configuration k0s manages, if this is a k0s
// node; k0s imports containerd.d beside it, and reloads containerd when
// a file there changes.
var k0sConfig = "/etc/k0s/containerd.toml"

// registerK0s writes gcr's drop-in into k0s's containerd.d, which k0s
// imports and applies without a restart while it manages the
// configuration (its first line says so).
func registerK0s(shim string, say func(string, ...any)) (bool, error) {
	b, err := os.ReadFile(k0sConfig)
	if err != nil {
		return false, err
	}
	if !bytes.HasPrefix(b, []byte("# k0s_managed=true")) {
		return false, fmt.Errorf("%s is not managed by k0s any more (no # k0s_managed=true first line), so k0s does not import containerd.d: "+
			"register runtime %s there yourself, or remove it and restart k0s to have k0s manage it again", k0sConfig, RuntimeType)
	}
	dropIn := filepath.Join(filepath.Dir(k0sConfig), "containerd.d", "gcr.toml")
	changed, err := writeIfChanged(dropIn, []byte(containerdDropIn(3, shim)), 0o644)
	if err != nil {
		return false, err
	}
	if changed {
		say("wrote %s; k0s reloads containerd with it", dropIn)
	}
	return changed, nil
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
