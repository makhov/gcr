//go:build linux

// containerd-shim-gcr-v2 is the gcr runtime: a containerd shim that runs
// containers in-process through libcontainer, with no runc binary.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/configs"
	_ "github.com/opencontainers/runc/libcontainer/nsenter"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/landlock"
	"github.com/makhov/gcr/internal/manager"
	"github.com/makhov/gcr/internal/setup"
	_ "github.com/makhov/gcr/internal/task"
	"github.com/makhov/gcr/internal/timetravel"
)

func init() {
	// The CRIU gcr setup installed, if it did, ahead of any other.
	setup.UseOwnCRIU()
	// libcontainer re-executes this binary as the container's init.
	// nsenter's constructor has already set up namespaces by now.
	if len(os.Args) > 1 && os.Args[1] == "init" {
		libcontainer.InitHook = initHook
		libcontainer.Init()
	}
	// The shim runs criu through itself to set up criu's process first.
	if len(os.Args) > 1 && os.Args[1] == container.MemoryMergeExec {
		err := container.ExecMerged(os.Args[2:])
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Time travel traces a task from a process of its own.
	if len(os.Args) > 1 && os.Args[1] == timetravel.StopExecArg {
		timetravel.StopExecMain(os.Args[2:])
	}
}

func initHook(c *configs.Config, keep []*os.File) error {
	// Before Landlock, which may leave the init unable to do anything
	// but exec.
	if err := container.MemoryMergeInitHook(c, keep); err != nil {
		return err
	}
	return landlock.InitHook(c, keep)
}

func main() {
	finish, err := adaptStart()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	shim.RunShim(context.Background(), manager.New("io.containerd.gcr.v2"), func(c *shim.Config) {
		// Containers are children of this process and are waited on by
		// libcontainer; an implicit reaper would steal their exit status.
		c.NoReaper = true
	})
	if finish != nil {
		if err := finish(); err != nil {
			fmt.Fprintln(os.Stderr, "start:", err)
			os.Exit(1)
		}
	}
}
