//go:build linux

// containerd-shim-gcr-v2 is the gcr runtime: a containerd shim that runs
// containers in-process through libcontainer, with no runc binary.
package main

import (
	"context"
	"os"

	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/opencontainers/runc/libcontainer"
	_ "github.com/opencontainers/runc/libcontainer/nsenter"

	"github.com/makhov/gcr/internal/landlock"
	"github.com/makhov/gcr/internal/manager"
	_ "github.com/makhov/gcr/internal/task"
)

func init() {
	// libcontainer re-executes this binary as the container's init.
	// nsenter's constructor has already set up namespaces by now.
	if len(os.Args) > 1 && os.Args[1] == "init" {
		libcontainer.InitHook = landlock.InitHook
		libcontainer.Init()
	}
}

func main() {
	shim.RunShim(context.Background(), manager.New("io.containerd.gcr.v2"), func(c *shim.Config) {
		// Containers are children of this process and are waited on by
		// libcontainer; an implicit reaper would steal their exit status.
		c.NoReaper = true
	})
}
