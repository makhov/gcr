//go:build linux

package container

import (
	"bytes"
	"fmt"
	"os"
	"runtime"

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
