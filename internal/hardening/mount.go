//go:build linux

package hardening

import (
	"context"
	"syscall"

	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/nsexec"
	"github.com/makhov/gcr/internal/supervisor"
)

// MountFilesystems are the filesystem types a container may mount (what
// Docker-in-Docker and nested runtimes need). Others the kernel would
// allow in a user namespace (ramfs without a size limit, fuse, ...) are
// refused.
var MountFilesystems = []string{"proc", "sysfs", "tmpfs", "devpts", "mqueue", "cgroup2", "overlay"}

// Operations on existing mounts, which need no filesystem type.
const mountOperations = unix.MS_REMOUNT | unix.MS_BIND | unix.MS_MOVE |
	unix.MS_SHARED | unix.MS_PRIVATE | unix.MS_SLAVE | unix.MS_UNBINDABLE

// HandleMount is the supervisor handler for mount(2). It copies the
// arguments, checks them, and performs the mount itself, as the caller
// and in its namespaces: letting the call continue would let another
// thread change the strings after the check.
func HandleMount(ctx context.Context, c *supervisor.Call) {
	source, err1 := c.ReadString(c.Args[0], 4096)
	target, err2 := c.ReadString(c.Args[1], 4096)
	fstype, err3 := c.ReadString(c.Args[2], 256)
	data, err4 := c.ReadString(c.Args[4], 4096)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		c.Reply(0, unix.EFAULT)
		return
	}
	flags := uintptr(c.Args[3])
	if errno := checkMount(fstype, flags); errno != 0 {
		log.G(ctx).WithField("fstype", fstype).WithField("flags", flags).Info("mount refused")
		c.Reply(0, errno)
		return
	}
	args := nsexec.MountArgs{Target: &target, Flags: flags}
	if c.Args[0] != 0 {
		args.Source = &source
	}
	if c.Args[2] != 0 {
		args.FSType = &fstype
	}
	if c.Args[4] != 0 {
		args.Data = &data
	}
	errno, err := nsexec.Mount(c.Pid, args)
	if err != nil {
		log.G(ctx).WithError(err).Warn("mount on behalf of container")
	}
	c.Reply(0, errno)
}

// checkMount allows operations on existing mounts and new mounts of the
// allowed filesystems.
func checkMount(fstype string, flags uintptr) syscall.Errno {
	if flags&mountOperations != 0 {
		return 0
	}
	for _, fs := range MountFilesystems {
		if fstype == fs {
			return 0
		}
	}
	return unix.EPERM
}
