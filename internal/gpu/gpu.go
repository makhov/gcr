//go:build linux

// Package gpu describes the NVIDIA GPUs a container uses, for its
// checkpoints: which GPUs it had (by device minor), on which driver, so a
// restore can tell whether the node it lands on can take it.
//
// GPUs get into containers through CDI (the NVIDIA device plugin and
// nvidia-ctk's generated spec): containerd puts the device nodes, driver
// libraries and hooks in the OCI spec, so they are part of what a restore
// rebuilds. CUDA state is moved to host memory before the dump and back
// after the restore by CRIU's CUDA plugin, which must be installed with
// CRIU. It calls the driver's own checkpoint API (cuCheckpointProcess*,
// r580 and newer), in the libcuda the container uses (UseLibCUDA).
//
// Restores need the same driver version (the restored processes have its
// libraries mapped, by path and content) and the same GPU models. They also
// need the same device minors: libcuda opens /dev/nvidia<minor> for the
// minor the driver reports, and the container's /dev is a set of mounts CRIU
// rebuilds exactly, so a container cannot move to another GPU index yet.
// GPUs may change identity (UUID), as on another node: CRIU's CUDA plugin
// maps them by ordinal.
package gpu

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// File is where a checkpoint records its GPUs, next to CRIU's images.
const File = "gcr-gpus.json"

// MinDriver is the oldest NVIDIA driver branch gcr checkpoints GPU
// containers on. From r580, CRIU's CUDA plugin checkpoints through the
// driver's API. Before, the plugin falls back to NVIDIA's cuda-checkpoint
// tool, which gcr neither installs nor is tested with.
const MinDriver = 580

// RestoreCRIUOptions are CRIU config lines for restoring a container with
// GPUs: its GPUs may have other UUIDs than at the dump (another node).
// Restore only: CRIU's CUDA plugin refuses a device map when dumping.
var RestoreCRIUOptions = []string{"plugin-option cuda_plugin.device-map=auto"}

// GPU is one GPU a container uses.
type GPU struct {
	Minor int    `json:"minor"`
	UUID  string `json:"uuid"`
	Model string `json:"model"`
	Bus   string `json:"bus"`
}

// Info is the GPUs a container uses and the driver serving them.
type Info struct {
	Driver string `json:"driver"`
	GPUs   []GPU  `json:"gpus"`
}

var minorPath = regexp.MustCompile(`^/dev/nvidia([0-9]+)$`)

// Minors are the GPU device minors in spec's devices (/dev/nvidia<minor>,
// as CDI puts them), sorted; none for a container without GPUs.
func Minors(spec *specs.Spec) []int {
	if spec.Linux == nil {
		return nil
	}
	var minors []int
	for _, d := range spec.Linux.Devices {
		if m := minorPath.FindStringSubmatch(d.Path); m != nil {
			n, _ := strconv.Atoi(m[1])
			if !slices.Contains(minors, n) {
				minors = append(minors, n)
			}
		}
	}
	slices.Sort(minors)
	return minors
}

// Probe describes the GPUs with the given minors from the driver's /proc
// files under root ("/" on a host).
func Probe(root string, minors []int) (*Info, error) {
	driver, err := Driver(root)
	if err != nil {
		return nil, err
	}
	info := &Info{Driver: driver}
	proc := filepath.Join(root, "proc/driver/nvidia")
	dirs, err := filepath.Glob(filepath.Join(proc, "gpus/*/information"))
	if err != nil {
		return nil, err
	}
	byMinor := map[int]GPU{}
	for _, path := range dirs {
		g, err := readInformation(path)
		if err != nil {
			return nil, err
		}
		g.Bus = filepath.Base(filepath.Dir(path))
		byMinor[g.Minor] = g
	}
	for _, m := range minors {
		g, ok := byMinor[m]
		if !ok {
			return nil, fmt.Errorf("no NVIDIA GPU with device minor %d", m)
		}
		info.GPUs = append(info.GPUs, g)
	}
	return info, nil
}

// Driver is the version of the NVIDIA driver loaded under root ("/" on a
// host), such as 580.65.06.
func Driver(root string) (string, error) {
	path := filepath.Join(root, "proc/driver/nvidia/version")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("NVIDIA driver: %w", err)
	}
	v := driverVersion(string(b))
	if v == "" {
		return "", fmt.Errorf("NVIDIA driver: no version in %s", path)
	}
	return v, nil
}

// Supported says whether gcr can checkpoint GPU containers on driver.
func Supported(driver string) error {
	branch, _, _ := strings.Cut(driver, ".")
	n, err := strconv.Atoi(branch)
	if err != nil {
		return fmt.Errorf("NVIDIA driver %q: not a version", driver)
	}
	if n < MinDriver {
		return fmt.Errorf("NVIDIA driver %s: checkpoints of GPU containers need r%d or newer: %w", driver, MinDriver, errdefs.ErrNotImplemented)
	}
	return nil
}

// LibCUDA is the host's libcuda that spec mounts into the container, ""
// without one. CDI mounts the driver's libraries from wherever the driver
// is: the host's library directory, or a driver container's root such as
// the GPU Operator's /run/nvidia/driver, which no host program finds.
func LibCUDA(spec *specs.Spec) string {
	for _, m := range spec.Mounts {
		if strings.HasPrefix(filepath.Base(m.Destination), "libcuda.so.") && m.Source != "" {
			return m.Source
		}
	}
	return ""
}

// UseLibCUDA makes the CRIUs this process starts load lib as libcuda.so.1,
// as CRIU's CUDA plugin does: the driver the container uses, wherever it
// is installed. lib is linked from a directory of its own under dir, put
// first on LD_LIBRARY_PATH; nothing else is there, so nothing else CRIU
// loads changes.
func UseLibCUDA(dir, lib string) error {
	sum := sha256.Sum256([]byte(lib))
	d := filepath.Join(dir, fmt.Sprintf("%x", sum[:8]))
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	link := filepath.Join(d, "libcuda.so.1")
	if cur, err := os.Readlink(link); err != nil || cur != lib {
		// Through a rename: another shim may be doing the same.
		tmp := fmt.Sprintf("%s.%d", link, os.Getpid())
		os.Remove(tmp)
		if err := os.Symlink(lib, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, link); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	path := os.Getenv("LD_LIBRARY_PATH")
	if slices.Contains(filepath.SplitList(path), d) {
		return nil
	}
	if path != "" {
		d += string(os.PathListSeparator) + path
	}
	return os.Setenv("LD_LIBRARY_PATH", d)
}

// driverVersion finds the version in /proc/driver/nvidia/version's first
// line ("NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  570.86.15
// Release Build ...").
func driverVersion(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	for _, f := range strings.Fields(line) {
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' && strings.Count(f, ".") >= 1 {
			return f
		}
	}
	return ""
}

// readInformation parses a /proc/driver/nvidia/gpus/<bus>/information file
// ("Model: \t\t NVIDIA A100 ...", "GPU UUID: \t GPU-...", "Device Minor: \t 0").
func readInformation(path string) (GPU, error) {
	f, err := os.Open(path)
	if err != nil {
		return GPU{}, err
	}
	defer f.Close()
	g := GPU{Minor: -1}
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, ok := strings.Cut(s.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "Model":
			g.Model = value
		case "GPU UUID":
			g.UUID = value
		case "Device Minor":
			g.Minor, _ = strconv.Atoi(value)
		}
	}
	if g.Minor < 0 {
		return g, fmt.Errorf("%s: no device minor", path)
	}
	return g, s.Err()
}

// Save records info in a checkpoint's directory.
func (info *Info) Save(dir string) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, File), b, 0o600)
}

// Load reads the GPUs a checkpoint recorded; nil when it had none.
func Load(dir string) (*Info, error) {
	b, err := os.ReadFile(filepath.Join(dir, File))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	return &info, nil
}

// Compatible says whether a container checkpointed with saved can be
// restored with now (nil: no GPUs).
func Compatible(saved, now *Info) error {
	switch {
	case saved == nil && now == nil:
		return nil
	case saved == nil:
		return nil // a CPU checkpoint does not care about GPUs it is given
	case now == nil:
		return fmt.Errorf("checkpoint used %d GPU(s), the container has none: %w", len(saved.GPUs), errdefs.ErrFailedPrecondition)
	}
	if saved.Driver != now.Driver {
		return fmt.Errorf("checkpoint was taken on NVIDIA driver %s, this node has %s: %w", saved.Driver, now.Driver, errdefs.ErrFailedPrecondition)
	}
	if len(saved.GPUs) != len(now.GPUs) {
		return fmt.Errorf("checkpoint used %d GPU(s), the container has %d: %w", len(saved.GPUs), len(now.GPUs), errdefs.ErrFailedPrecondition)
	}
	for i, s := range saved.GPUs {
		n := now.GPUs[i]
		if s.Model != n.Model {
			return fmt.Errorf("checkpoint used a %s, the container has a %s: %w", s.Model, n.Model, errdefs.ErrFailedPrecondition)
		}
		if s.Minor != n.Minor {
			return fmt.Errorf("checkpoint used GPU device minor %d, the container has %d; restoring onto another GPU index is not supported yet: %w",
				s.Minor, n.Minor, errdefs.ErrNotImplemented)
		}
	}
	return nil
}
