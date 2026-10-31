//go:build linux

package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/landlock"
	"github.com/makhov/gcr/internal/netpolicy"
	"github.com/makhov/gcr/internal/nydus"
	"github.com/makhov/gcr/internal/payload"
)

// Level is how a check came out.
type Level int

const (
	OK Level = iota
	// Optional: a feature is off on this node, which the rest does not
	// need.
	Optional
	// Missing: gcr does not work here as it should.
	Missing
)

// Check is one thing Doctor looked at.
type Check struct {
	Name   string
	Level  Level
	Detail string
	// Fix says what to do about it, if it is not OK.
	Fix string
}

// Doctor checks the node for what gcr and its features need. It looks at
// the CRIU the shim runs: gcr's, if setup installed one.
func Doctor() []Check {
	UseOwnCRIU()
	var cs []Check
	add := func(name string, lvl Level, detail, fix string) {
		cs = append(cs, Check{Name: name, Level: lvl, Detail: detail, Fix: fix})
	}

	// The node.
	var u unix.Utsname
	_ = unix.Uname(&u)
	release := unix.ByteSliceToString(u.Release[:])
	if kernelAtLeast(release, 6, 12) {
		add("kernel", OK, release, "")
	} else {
		add("kernel", Missing, release, "gcr needs Linux 6.12 or later")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		add("cgroup v2", OK, "unified hierarchy", "")
	} else {
		add("cgroup v2", Missing, "not mounted at /sys/fs/cgroup", "boot with cgroup v2 (systemd.unified_cgroup_hierarchy=1)")
	}

	// The runtime.
	// Where the registration names it (setup -bin-dir), or on PATH.
	path := registeredShim()
	if path == "" {
		path, _ = exec.LookPath(payload.Shim)
	}
	if path == "" {
		add("shim", Missing, "not registered or in PATH", "gcr setup")
	} else {
		v, _ := exec.Command(path, "-v").Output()
		add("shim", OK, path+" "+field(string(v), "Version:"), "")
	}
	// containerd github.com/containerd/containerd/v2 v2.4.2 <revision>
	out, err := exec.Command("containerd", "--version").Output()
	which := ""
	if f := strings.Fields(string(out)); len(f) >= 3 {
		which = f[2]
	}
	if _, k0s := os.Stat(k0sConfig); k0s == nil {
		which, err = "k0s's", nil // its own, off PATH
	} else if d := detectDistro(); d != nil {
		which, err = d.Name+"'s", nil
	}
	if err != nil {
		add("containerd", Missing, "not found", "install containerd 1.7 or later")
	} else if registered() {
		add("containerd", OK, which+", runtime gcr registered", "")
	} else {
		add("containerd", Missing, "runtime gcr not registered for Kubernetes", "gcr setup (ctr needs nothing: --runtime "+RuntimeType+")")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{json .Runtimes}}").Output(); err == nil {
		var rt map[string]any
		if json.Unmarshal(out, &rt) == nil && rt["gcr"] != nil {
			add("docker", OK, "runtime gcr registered", "")
		} else {
			add("docker", Optional, "runtime gcr not registered", "gcr setup -docker")
		}
	}

	// CRIU: checkpoints, forks, time travel, migration.
	criu, err := exec.LookPath("criu")
	switch {
	case err != nil:
		add("CRIU", Missing, "not found: no checkpoints, forks, time travel or migration", "a release's gcr setup installs gcr's; or make criu from makhov/criu")
	default:
		v, verr := exec.Command(criu, "--version").Output()
		if verr != nil {
			// gcr's needs glibc: it does not start on musl (Alpine).
			add("CRIU", Optional, criu+" does not run here: no checkpoints, forks, time travel or migration",
				"it needs glibc 2.35 or later; on other nodes, build CRIU for them (make criu)")
			break
		}
		help, _ := exec.Command(criu, "--help").CombinedOutput()
		var lacks []string
		for flag, what := range map[string]string{
			"--seccomp-notify-socket": "time travel", "--landlock-fs": "Landlock across restores",
			"--lazy-pages-on-demand": "forks holding only the memory they touch",
		} {
			if !bytes.Contains(help, []byte(flag)) {
				lacks = append(lacks, what)
			}
		}
		detail := criu + " " + field(string(v), "Version:")
		if id := field(string(v), "GitID:"); id != "" {
			detail += " (" + id + ")"
		}
		if len(lacks) > 0 {
			add("CRIU", Optional, detail+", not gcr's: no "+strings.Join(lacks, ", "), "a release's gcr setup installs gcr's; or make criu from makhov/criu")
		} else if out, err := exec.Command(criu, "check").CombinedOutput(); err != nil {
			add("CRIU", Missing, detail+": criu check fails", lastLine(string(out)))
		} else {
			add("CRIU", OK, detail, "")
		}
	}

	// Isolation.
	if abi := landlock.ABI(); abi > 0 {
		add("Landlock", OK, fmt.Sprintf("ABI %d", abi), "")
	} else {
		add("Landlock", Optional, "not in the kernel: containers are not confined to their root", "boot with landlock in lsm=")
	}
	if netpolicy.Available() {
		add("netlink policy", OK, "BPF LSM", "")
	} else {
		fix := "add bpf to the kernel's lsm= list and reboot"
		if lsm, _ := os.ReadFile("/sys/kernel/security/lsm"); len(bytes.TrimSpace(lsm)) > 0 {
			fix = "add bpf to the kernel's lsm= list (" + strings.TrimSpace(string(lsm)) + ") and reboot"
		}
		add("netlink policy", Optional, "no BPF LSM: containers' network configuration is limited by seccomp alone", fix)
	}

	// Features that depend on the node.
	if container.LazyPagesAvailable() {
		add("lazy restore", OK, "vm.unprivileged_userfaultfd = 1", "")
	} else {
		add("lazy restore", Optional, "vm.unprivileged_userfaultfd = 0: no gcr.dev/lazy-restore or post-copy migration for containers in a user namespace",
			"sysctl vm.unprivileged_userfaultfd=1 (docs/fork.md#lazy-restore says what it exposes)")
	}
	if err == nil {
		if container.MemoryTracking() {
			add("dirty memory", OK, "tracked: pre-copy migration, incremental time travel points", "")
		} else {
			add("dirty memory", Optional, "not tracked (arm64): migration is stop-and-copy or post-copy, time travel points are full", "")
		}
	}
	if err := container.MemoryMergeAvailable(); err == nil {
		add("memory merge", OK, "KSM on", "")
	} else {
		add("memory merge", Optional, err.Error()+": no gcr.dev/memory-merge", "echo 1 > /sys/kernel/mm/ksm/run")
	}
	if err := nydus.Available(); err == nil {
		add("nydus", OK, "snapshots in the nydus format, lazy pulls", "")
	} else {
		add("nydus", Optional, "no nydus-image or nydusd: snapshots in the tar format only", "install nydus 2.4 (github.com/dragonflyoss/nydus)")
	}
	if _, err := os.Stat("/dev/nvidia0"); err == nil {
		if _, err := os.Stat(filepath.Join(pluginDir(), payload.CUDAPlugin)); err == nil {
			add("GPUs", OK, "CRIU's CUDA plugin installed", "")
		} else {
			add("GPUs", Missing, "NVIDIA GPUs, but no CRIU CUDA plugin: no checkpoints of GPU containers", "a release's gcr setup installs it; or make criu")
		}
	}
	return cs
}

var runtimePath = regexp.MustCompile(`(?m)^\s*runtime_path\s*=\s*['"]([^'"]+)['"]`)

// registeredShim is the shim gcr setup's drop-in names, if it exists.
func registeredShim() string {
	candidates := []string{"/etc/containerd/conf.d/gcr.toml", filepath.Join(filepath.Dir(k0sConfig), "containerd.d", "gcr.toml")}
	for _, d := range distros {
		candidates = append(candidates, filepath.Join(d.Dir, "config-v3.toml.d", "gcr.toml"), filepath.Join(d.Dir, "config.toml.d", "gcr.toml"))
	}
	for _, f := range candidates {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if m := runtimePath.FindSubmatch(b); m != nil {
			if _, err := os.Stat(string(m[1])); err == nil {
				return string(m[1])
			}
		}
	}
	return ""
}

// registered says whether containerd's configuration names the runtime.
func registered() bool {
	files, _ := filepath.Glob("/etc/containerd/conf.d/*.toml")
	k0s, _ := filepath.Glob("/etc/k0s/containerd.d/*.toml")
	files = append(append(files, k0s...), "/etc/containerd/config.toml", k0sConfig)
	for _, d := range distros {
		dropIns, _ := filepath.Glob(filepath.Join(d.Dir, "*.toml.d", "*.toml"))
		files = append(append(files, dropIns...), filepath.Join(d.Dir, "config.toml"))
	}
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte(RuntimeType)) {
			return true
		}
	}
	return false
}

func pluginDir() string {
	if d := os.Getenv("CRIU_LIBS_DIR"); d != "" {
		return d
	}
	if _, err := os.Stat(PluginDir); err == nil {
		return PluginDir
	}
	return "/usr/lib/criu"
}

func kernelAtLeast(release string, major, minor int) bool {
	f := strings.FieldsFunc(release, func(r rune) bool { return r == '.' || r == '-' })
	if len(f) < 2 {
		return false
	}
	ma, _ := strconv.Atoi(f[0])
	mi, _ := strconv.Atoi(f[1])
	return ma > major || (ma == major && mi >= minor)
}

func field(s, key string) string {
	for _, l := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), key); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
