//go:build linux

// Package nsexec runs operations inside another process's namespaces and
// with its credentials, through a helper the shim re-executes (the C
// constructor in nsexec.c runs it before the Go runtime starts).
package nsexec

// #cgo CFLAGS: -Wall
import "C"

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// MountArgs are mount(2)'s arguments; nil pointers stay NULL.
type MountArgs struct {
	Source, Target, FSType, Data *string
	Flags                        uintptr
}

// Mount performs a mount as process pid would: in its namespaces, root and
// working directory, with its IDs and effective capabilities. It returns
// the errno the mount failed with, or 0.
func Mount(pid int, a MountArgs) (syscall.Errno, error) {
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = []string{
		"_GCR_NSEXEC=mount",
		"_GCR_PID=" + strconv.Itoa(pid),
		"_GCR_FLAGS=" + strconv.FormatUint(uint64(a.Flags), 10),
	}
	for name, v := range map[string]*string{"_GCR_SOURCE": a.Source, "_GCR_TARGET": a.Target, "_GCR_FSTYPE": a.FSType, "_GCR_DATA": a.Data} {
		if v != nil {
			cmd.Env = append(cmd.Env, name+"=+"+*v)
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit) && exit.ExitCode() > 0:
		return syscall.Errno(exit.ExitCode()), nil
	default:
		return syscall.EPERM, fmt.Errorf("mount helper: %w", err)
	}
}
