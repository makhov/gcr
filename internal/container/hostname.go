//go:build linux

package container

import (
	"bytes"
	"fmt"
	"os"
	"runtime"

	"github.com/containerd/errdefs"
	"github.com/opencontainers/runc/libcontainer/configs"
	"golang.org/x/sys/unix"
)

// hostnameOf returns the hostname of the UTS namespace at path.
func hostnameOf(path string) (string, error) {
	var name string
	err := inUTS(path, func() error {
		var u unix.Utsname
		if err := unix.Uname(&u); err != nil {
			return err
		}
		b, _, _ := bytes.Cut(u.Nodename[:], []byte{0})
		name = string(b)
		return nil
	})
	return name, err
}

// setHostname sets the hostname of process pid's UTS namespace.
func setHostname(pid int, name string) error {
	return inUTS(fmt.Sprintf("/proc/%d/ns/uts", pid), func() error {
		return unix.Sethostname([]byte(name))
	})
}

// inUTS runs fn on a thread in the UTS namespace at path.
func inUTS(path string, fn func() error) error {
	ns, err := os.Open(path)
	if err != nil {
		return err
	}
	defer ns.Close()
	errc := make(chan error, 1)
	go func() {
		// The thread ends in another namespace: it is not unlocked, so
		// it exits with the goroutine rather than serve another.
		runtime.LockOSThread()
		if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWUTS); err != nil {
			errc <- fmt.Errorf("enter %s: %w", path, err)
			return
		}
		errc <- fn()
	}()
	return <-errc
}

// checkRestoredNetwork refuses a restore that left the container's init
// outside the network namespace its spec joins: CRIU leaves a process
// whose images were dumped in its own network namespace (the host's) in
// the one it runs in, which would give a pod the node's network.
func (c *Container) checkRestoredNetwork(proc interface{ Pid() (int, error) }) error {
	want := c.cfg.Namespaces.PathOf(configs.NEWNET)
	if want == "" {
		return nil
	}
	pid, err := proc.Pid()
	if err != nil {
		return err
	}
	same, err := sameNamespace(want, fmt.Sprintf("/proc/%d/ns/net", pid))
	if err != nil {
		return err
	}
	if !same {
		_ = unix.Kill(pid, unix.SIGKILL)
		return fmt.Errorf("its processes are not in the network namespace it joins (%s): "+
			"a snapshot taken in the host's network cannot be restored into a pod's: %w", want, errdefs.ErrFailedPrecondition)
	}
	return nil
}

func sameNamespace(a, b string) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino, nil
}
