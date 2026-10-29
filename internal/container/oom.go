//go:build linux

package container

import (
	"os"
	"path/filepath"
	"time"

	"github.com/opencontainers/cgroups/fs2"
)

// OOMFile is written into the bundle when the kernel's OOM killer kills
// one of the container's processes. CRI-O's runtimes of type "vm" report
// a container OOMKilled by it; containerd learns of it from OnOOM's
// event.
const OOMFile = "oom"

// watchOOM notes OOM kills while the container runs.
func (c *Container) watchOOM() {
	if dir, err := c.CgroupPath(); err == nil {
		c.mu.Lock()
		c.cgroupDir = dir
		c.mu.Unlock()
	}
	ch, err := c.lc.NotifyOOM()
	if err != nil {
		return // no cgroup v2 memory controller: noted at exit, if at all
	}
	watched := make(chan struct{}, 1)
	c.mu.Lock()
	c.oomWatched = watched
	c.mu.Unlock()
	go func() {
		for range ch {
			// libcontainer read the count as it changed. By now the
			// cgroup may be gone (systemd removes a scope as soon as it is
			// empty), but a kill it signalled happened.
			c.noteOOM(true)
			select {
			case watched <- struct{}{}:
			default:
			}
		}
	}()
}

// noteOOM tells of OOM kills the container's cgroup counts and were not
// told yet, or of one if signalled and the cgroup is gone. It returns
// whether any kill has been told of.
func (c *Container) noteOOM(signalled bool) bool {
	c.mu.Lock()
	dir := c.cgroupDir
	c.mu.Unlock()
	var err error
	if dir == "" {
		dir, err = c.CgroupPath()
	}
	var n uint64
	if err == nil {
		n, err = fs2.OOMKillCount(dir)
	}
	c.mu.Lock()
	seen := c.oomKills
	if err != nil {
		if !signalled {
			c.mu.Unlock()
			return seen > 0
		}
		n = seen + 1
	}
	if n > seen {
		c.oomKills = n
	}
	c.mu.Unlock()
	if n > seen {
		_ = os.WriteFile(filepath.Join(c.Bundle, OOMFile), nil, 0o644)
		if c.onOOM != nil {
			c.onOOM(c)
		}
	}
	return n > 0
}

// noteOOMAtExit runs before an exit is reported, so the OOM kill that
// ended the init is told of first. A process killed by SIGKILL whose
// cgroup is already gone may have been the OOM killer's, with the watcher
// not yet woken: give it a moment.
func (c *Container) noteOOMAtExit(killed bool) {
	c.mu.Lock()
	watched := c.oomWatched
	c.mu.Unlock()
	if c.noteOOM(false) || !killed || watched == nil {
		return
	}
	select {
	case <-watched:
	case <-time.After(500 * time.Millisecond):
	}
}
