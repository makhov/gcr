//go:build linux

package container

import (
	"cmp"
	"fmt"
	"runtime"
	"slices"
	"strings"

	"github.com/containerd/errdefs"
	pathrs "github.com/cyphar/filepath-securejoin/pathrs-lite"
	"github.com/opencontainers/runc/libcontainer/configs"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/gpu"
)

// gpuMoves pairs the snapshot's GPUs with this container's in bus order,
// which is how the CUDA plugin's automatic device map pairs them (each
// side numbers its GPUs as CUDA does, by bus), and gives the device
// minors that change, old to new. A restore onto other GPUs is one onto
// other /dev/nvidia<minor>: CRIU rebuilds the snapshot's /dev, and libcuda
// opens the node named for the minor of the GPU it now has.
func gpuMoves(saved, now *gpu.Info) map[int]int {
	if saved == nil || now == nil || len(saved.GPUs) != len(now.GPUs) {
		return nil
	}
	byBus := func(a, b gpu.GPU) int { return cmp.Compare(a.Bus, b.Bus) }
	s := slices.SortedFunc(slices.Values(saved.GPUs), byBus)
	n := slices.SortedFunc(slices.Values(now.GPUs), byBus)
	moves := map[int]int{}
	for i := range s {
		if s[i].Minor != n[i].Minor {
			moves[s[i].Minor] = n[i].Minor
		}
	}
	return moves
}

// gpuMounts are, for a container in a user namespace, the CRIU options
// that bind the snapshot's GPU device mounts to this container's devices:
// there a device is a bind mount from the host, which CRIU restores at the
// snapshot's path, /dev/nvidia<old>. The DRI nodes of the GPUs are paired
// in name order. had are the bind mounts in the images.
func gpuMounts(moves map[int]int, had map[string]bool, cfg *configs.Config) ([]string, error) {
	var opts []string
	for old, n := range moves {
		if _, ok := moves[n]; ok || had[nvidia(n)] {
			return nil, fmt.Errorf("the snapshot's GPU /dev/nvidia%d is this container's /dev/nvidia%d, and its /dev/nvidia%d another; in a user namespace a restore moves GPUs only to new device minors: %w",
				old, n, n, errdefs.ErrNotImplemented)
		}
		opts = append(opts, fmt.Sprintf("ext-mount-map %s:%s", nvidia(old), nvidia(n)))
	}
	for _, kind := range []string{"/dev/dri/card", "/dev/dri/renderD"} {
		var gone, added []string
		for k := range had {
			if strings.HasPrefix(k, kind) && !isDevice(cfg, k) {
				gone = append(gone, k)
			}
		}
		for _, d := range cfg.Devices {
			if p := inRootfs(cfg.Rootfs, d.Path); strings.HasPrefix(p, kind) && !had[p] {
				added = append(added, d.Path)
			}
		}
		if len(gone) != len(added) {
			continue // checkMounts says which
		}
		slices.Sort(gone)
		slices.Sort(added)
		for i := range gone {
			opts = append(opts, fmt.Sprintf("ext-mount-map %s:%s", gone[i], added[i]))
		}
	}
	slices.Sort(opts)
	return opts, nil
}

// gpuMounted are the image mounts gpuMounts binds to other devices.
func gpuMounted(opts []string) map[string]bool {
	m := map[string]bool{}
	for _, o := range opts {
		k, _, _ := strings.Cut(strings.TrimPrefix(o, "ext-mount-map "), ":")
		m[k] = true
	}
	return m
}

// gpuNodes gives the restored container, before its CUDA state is
// restored, the /dev/nvidia<new> libcuda will open. In a user namespace
// the snapshot's /dev/nvidia<old> is already this GPU (gpuMounts), and
// the new name links to it: a device node on the container's own /dev
// cannot be opened there. Otherwise /dev/nvidia<new> is created as the
// host's, and the snapshot's nodes for GPUs this container does not have
// go. root are the host ids of the container's root in its user namespace,
// nil without one: the link is made as that root, since its /dev cannot
// hold a file owned by an id the namespace does not map.
func gpuNodes(moves map[int]int, root *[2]int) func(pid int) error {
	userns := root != nil
	return func(pid int) error {
		dev, err := pathrs.OpenInRoot(fmt.Sprintf("/proc/%d/root", pid), "dev")
		if err != nil {
			return fmt.Errorf("restored /dev: %w", err)
		}
		defer dev.Close()
		fd := int(dev.Fd())
		now := map[int]bool{}
		for _, n := range moves {
			now[n] = true
		}
		if !userns {
			for old := range moves {
				if !now[old] {
					if err := unix.Unlinkat(fd, fmt.Sprintf("nvidia%d", old), 0); err != nil && err != unix.ENOENT {
						return fmt.Errorf("restored /dev/nvidia%d: %w", old, err)
					}
				}
			}
		}
		for old, n := range moves {
			name := fmt.Sprintf("nvidia%d", n)
			if err := unix.Unlinkat(fd, name, 0); err != nil && err != unix.ENOENT {
				return fmt.Errorf("restored /dev/%s: %w", name, err)
			}
			if userns {
				err = asIDs(root[0], root[1], func() error {
					return unix.Symlinkat(fmt.Sprintf("nvidia%d", old), fd, name)
				})
			} else {
				var st unix.Stat_t
				if err = unix.Stat("/dev/"+name, &st); err == nil {
					if err = unix.Mknodat(fd, name, st.Mode, int(st.Rdev)); err == nil {
						err = unix.Fchmodat(fd, name, st.Mode&0o7777, 0)
					}
				}
			}
			if err != nil {
				return fmt.Errorf("restored /dev/%s: %w", name, err)
			}
		}
		return nil
	}
}

// asIDs runs f with the calling thread's filesystem ids set to uid and gid.
func asIDs(uid, gid int, f func() error) error {
	runtime.LockOSThread()
	pgid, _ := unix.SetfsgidRetGid(gid)
	puid, _ := unix.SetfsuidRetUid(uid)
	err := f()
	unix.SetfsuidRetUid(puid)
	unix.SetfsgidRetGid(pgid)
	runtime.UnlockOSThread()
	return err
}

// hostRoot are the host ids of root in a container with user namespace
// mappings, nil without.
func hostRoot(cfg *configs.Config) *[2]int {
	id := func(m []configs.IDMap) int {
		for _, e := range m {
			if e.ContainerID == 0 {
				return int(e.HostID)
			}
		}
		return -1
	}
	if !cfg.Namespaces.Contains(configs.NEWUSER) {
		return nil
	}
	return &[2]int{id(cfg.UIDMappings), id(cfg.GIDMappings)}
}

func nvidia(minor int) string { return fmt.Sprintf("/dev/nvidia%d", minor) }
