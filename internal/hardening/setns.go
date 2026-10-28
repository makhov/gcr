//go:build linux

package hardening

import (
	"context"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/supervisor"
)

const (
	// nsGetNSType is ioctl NS_GET_NSTYPE on a namespace file.
	nsGetNSType = 0xb703
	// pidfdThread makes pidfd_open take a thread ID (PIDFD_THREAD).
	pidfdThread = unix.O_EXCL
)

// HandleSetns is the supervisor handler for setns(2). Joining the network
// namespace the caller is already in changes nothing, but the kernel still
// wants CAP_SYS_ADMIN over the namespace's owner, which a container in its
// own user namespace lacks for a pod network namespace the host created.
// dockerd does just that for host networking, so gcr answers that case
// itself. Every other call runs as made: the kernel checks it then, so a
// descriptor swapped in the meantime gains nothing.
func HandleSetns(ctx context.Context, c *supervisor.Call) {
	if ownNetwork(c) {
		c.Reply(0, 0)
		return
	}
	c.Continue()
}

// ownNetwork reports whether setns's descriptor is the caller's current
// network namespace.
func ownNetwork(c *supervisor.Call) bool {
	if nstype := c.Args[1]; nstype != 0 && nstype != unix.CLONE_NEWNET {
		return false
	}
	pidfd, err := unix.PidfdOpen(c.Pid, pidfdThread)
	if err != nil {
		return false
	}
	defer unix.Close(pidfd)
	fd, err := unix.PidfdGetfd(pidfd, int(int32(c.Args[0])), 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	if t, err := unix.IoctlRetInt(fd, nsGetNSType); err != nil || t != unix.CLONE_NEWNET {
		return false
	}
	var want, got unix.Stat_t
	if unix.Fstat(fd, &got) != nil || unix.Stat("/proc/"+strconv.Itoa(c.Pid)+"/ns/net", &want) != nil {
		return false
	}
	return got.Dev == want.Dev && got.Ino == want.Ino && c.Valid()
}
