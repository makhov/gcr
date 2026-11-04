//go:build linux

package gpu

import (
	"context"
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Tools are nvidia-smi and the directory of the libnvidia-ml it loads, as
// CDI mounted them into a container: from wherever the driver is, like
// LibCUDA. Either may be "": nvidia-smi is then looked up on PATH, and
// loads whichever libnvidia-ml the host's loader finds.
type Tools struct {
	SMI, LibDir string
}

// ToolsOf are the Tools spec mounts into the container.
func ToolsOf(spec *specs.Spec) Tools {
	var t Tools
	for _, m := range spec.Mounts {
		switch base := filepath.Base(m.Destination); {
		case m.Source == "":
		case base == "nvidia-smi":
			t.SMI = m.Source
		case strings.HasPrefix(base, "libnvidia-ml.so."):
			t.LibDir = filepath.Dir(m.Source)
		}
	}
	return t
}

// MemoryUsed is the GPU memory, in bytes, that the processes pids use, as
// nvidia-smi reports it: what a checkpoint has the driver copy into their
// host memory.
func MemoryUsed(t Tools, pids []int) (uint64, error) {
	smi := t.SMI
	if smi == "" {
		var err error
		if smi, err = exec.LookPath("nvidia-smi"); err != nil {
			return 0, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, smi, "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
	if t.LibDir != "" {
		cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+t.LibDir)
	}
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", smi, err)
	}
	return computeAppsMemory(string(out), pids)
}

// computeAppsMemory sums, in bytes, the used_memory (MiB) of pids in
// nvidia-smi's "pid, used_memory" lines.
func computeAppsMemory(out string, pids []int) (uint64, error) {
	ours := map[int]bool{}
	for _, p := range pids {
		ours[p] = true
	}
	var sum uint64
	for line := range strings.Lines(out) {
		pid, mib, ok := strings.Cut(strings.TrimSpace(line), ",")
		if !ok {
			continue
		}
		p, err := strconv.Atoi(strings.TrimSpace(pid))
		if err != nil || !ours[p] {
			continue
		}
		m, err := strconv.ParseUint(strings.TrimSpace(mib), 10, 64)
		if err != nil {
			// [N/A]: the driver does not say.
			return 0, fmt.Errorf("GPU memory of pid %d: %q", p, strings.TrimSpace(mib))
		}
		sum += m << 20
	}
	return sum, nil
}

// CustomStorage says whether lib, a libcuda, has the driver's custom
// storage (CUDA 13.4, r615 and newer), through which CRIU's CUDA plugin
// checkpoints GPU memory to an image file instead of the task's memory.
func CustomStorage(lib string) bool {
	f, err := elf.Open(lib)
	if err != nil {
		return false
	}
	defer f.Close()
	syms, err := f.DynamicSymbols()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(syms, func(s elf.Symbol) bool { return s.Name == "cuCheckpointOperationComplete" })
}
