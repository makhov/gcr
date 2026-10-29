//go:build linux

package container

import (
	"os"
	"path/filepath"

	"github.com/containerd/log"
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
	go func() {
		for range ch {
			c.noteOOM(false, false)
		}
	}()
}

// noteOOM tells of OOM kills the container's cgroup counts and were not
// told yet. It runs at exit as well, before the exit is reported: the
// watcher may not have seen the kill that ended the init by then.
func (c *Container) noteOOM(exiting, killed bool) {
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
	if exiting && killed {
		log.L.WithError(err).WithField("id", c.ID).WithField("cgroup", dir).WithField("oom_kill", n).Info("killed: OOM kills at exit")
	}
	if err != nil {
		return
	}
	c.mu.Lock()
	seen := c.oomKills
	if n > seen {
		c.oomKills = n
	}
	c.mu.Unlock()
	if n <= seen {
		return
	}
	_ = os.WriteFile(filepath.Join(c.Bundle, OOMFile), nil, 0o644)
	if c.onOOM != nil {
		c.onOOM(c)
	}
}
