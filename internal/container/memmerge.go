//go:build linux

package container

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/opencontainers/runc/libcontainer/configs"
	"golang.org/x/sys/unix"
)

// MemoryMergeExec is the shim's argument that runs the rest of its
// arguments with memory merging on (ExecMerged).
const MemoryMergeExec = "memory-merge-exec"

// MemoryMergeAvailable says why the kernel would not merge a container's
// memory, or nil if it would.
func MemoryMergeAvailable() error {
	b, err := os.ReadFile("/sys/kernel/mm/ksm/run")
	if err != nil {
		return fmt.Errorf("no KSM in this kernel: %w", err)
	}
	if strings.TrimSpace(string(b)) != "1" {
		return fmt.Errorf("KSM is not running (/sys/kernel/mm/ksm/run is %s, not 1)", strings.TrimSpace(string(b)))
	}
	return nil
}

// mergeMemory makes the calling process's memory mergeable, and that of
// every process it starts or execs from now on (Linux 6.4).
func mergeMemory() error {
	if err := unix.Prctl(unix.PR_SET_MEMORY_MERGE, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("PR_SET_MEMORY_MERGE: %w", err)
	}
	return nil
}

// ExecMerged execs args with memory merging on. The shim runs criu through
// it for a container that asked for merging: the tasks criu restores are
// its children and inherit it, which Go cannot set up between fork and
// exec.
func ExecMerged(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s: no command", MemoryMergeExec)
	}
	if err := mergeMemory(); err != nil {
		return err
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	return unix.Exec(path, args, os.Environ())
}

// MemoryMergeInitHook turns memory merging on in a container's init
// before it execs the container's process, if the container asked for
// it. It is (part of) libcontainer's InitHook.
func MemoryMergeInitHook(c *configs.Config, _ []*os.File) error {
	if !slices.Contains(c.Labels, AnnotationMemoryMerge+"=true") {
		return nil
	}
	return mergeMemory()
}

// criuCommand runs criu for a restore: through the shim, to turn memory
// merging on first, when the container asked for it.
func (c *Container) criuCommand() []string {
	if !c.memoryMerge {
		return nil
	}
	return []string{"/proc/self/exe", MemoryMergeExec, "criu"}
}
