//go:build linux

package container

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// OOMFile is written into the bundle when the kernel's OOM killer kills
// one of the container's processes. CRI-O's runtimes of type "vm" report
// a container OOMKilled by it; containerd learns of it from OnOOM's
// event.
const OOMFile = "oom"

// The kernel notifies memory.events at most every 10ms, and not at all
// once the cgroup is gone: under the systemd cgroup driver that is as soon
// as it is empty, which may be before the exit is handled. The first
// notification comes when the limit is hit (max), and the kill only after
// reclaim gives up, which takes as long as it takes. So after each
// notification the watcher reads it on while the counts move, until they
// have been still for oomQuiet. All of it can be over in a millisecond or
// two: the kill, the last process exiting, the cgroup removed. The
// parent's memory.events counts the kills in its children too and stays,
// so the exit also counts a kill the parent counted since the watch began.
const (
	oomInterval = 2 * time.Millisecond
	oomQuiet    = 100 * time.Millisecond
)

// memoryEvents reads the times the limit was hit, the OOM killer was
// invoked and killed, from the cgroup's memory.events.
func memoryEvents(dir string) (limit, oom, kills uint64, err error) {
	b, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return 0, 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(line, " ")
		n, _ := strconv.ParseUint(v, 10, 64)
		switch k {
		case "max":
			limit = n
		case "oom":
			oom = n
		case "oom_kill":
			kills = n
		}
	}
	return limit, oom, kills, nil
}

// watchOOM notes OOM kills while the container runs, from when its cgroup
// is made (Create, or a restore).
func (c *Container) watchOOM() {
	dir, err := c.CgroupPath()
	if err != nil {
		return // noted at exit, if at all
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return
	}
	if _, err := unix.InotifyAddWatch(fd, filepath.Join(dir, "memory.events"), unix.IN_MODIFY); err != nil {
		unix.Close(fd)
		return
	}
	_, _, parentKills, _ := memoryEvents(filepath.Dir(dir))
	c.mu.Lock()
	c.cgroupDir, c.parentKills = dir, parentKills
	c.mu.Unlock()
	c.readOOM(dir) // from before the watch
	go func() {
		defer unix.Close(fd)
		var buf [4096]byte
		for {
			if _, err := unix.Read(fd, buf[:]); err == unix.EINTR {
				continue
			} else if err != nil {
				return
			}
			var last [3]uint64
			for still := time.Duration(0); still < oomQuiet; still += oomInterval {
				limit, oom, kills, err := memoryEvents(dir)
				if err != nil {
					return // the cgroup is gone
				}
				if now := [3]uint64{limit, oom, kills}; now != last {
					last, still = now, 0
				}
				c.noteCounts(oom, kills)
				time.Sleep(oomInterval)
			}
		}
	}()
}

// readOOM reads the cgroup's counts and tells of new kills. It returns
// false if the cgroup is gone.
func (c *Container) readOOM(dir string) bool {
	_, oom, kills, err := memoryEvents(dir)
	if err != nil {
		return false
	}
	c.noteCounts(oom, kills)
	return true
}

func (c *Container) noteCounts(oom, kills uint64) {
	c.mu.Lock()
	c.oomInvoked = max(c.oomInvoked, oom)
	c.mu.Unlock()
	c.tellOOM(kills)
}

// tellOOM tells of the kills in count not told of yet.
func (c *Container) tellOOM(count uint64) {
	c.mu.Lock()
	seen := c.oomKills
	if count > seen {
		c.oomKills = count
	}
	c.mu.Unlock()
	if count <= seen {
		return
	}
	_ = os.WriteFile(filepath.Join(c.Bundle, OOMFile), nil, 0o644)
	if c.onOOM != nil {
		c.onOOM(c)
	}
}

// noteOOMAtExit runs before an exit is reported, so the OOM kill that
// ended it is told of first: read from the cgroup if it is still there.
// If not, a process killed (SIGKILL) counts as killed by the OOM killer if
// the watcher saw it invoked in the cgroup, or the parent counted a kill
// since the watch began. (Another child of the parent killed by it at
// that moment would be taken for this one.)
func (c *Container) noteOOMAtExit(killed bool) {
	c.mu.Lock()
	dir := c.cgroupDir
	c.mu.Unlock()
	if dir == "" {
		if d, err := c.CgroupPath(); err == nil {
			dir = d
		}
	}
	if c.readOOM(dir) || !killed {
		return
	}
	_, _, parentKills, perr := memoryEvents(filepath.Dir(dir))
	c.mu.Lock()
	unseen := c.oomKills == 0 && (c.oomInvoked > 0 || perr == nil && parentKills > c.parentKills)
	c.mu.Unlock()
	if unseen {
		c.tellOOM(1)
	}
}
