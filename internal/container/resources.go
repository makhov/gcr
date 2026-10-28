//go:build linux

package container

import (
	"fmt"
	"strings"

	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runc/libcontainer/specconv"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Stats returns the container's cgroup v2 metrics, in the format
// containerd's CRI plugin reads for kubelet.
func (c *Container) Stats() (*stats.Metrics, error) {
	c.op.Lock()
	st, err := c.lc.State()
	c.op.Unlock()
	if err != nil {
		return nil, err
	}
	path, ok := st.CgroupPaths[""]
	if !ok {
		return nil, fmt.Errorf("stats need cgroup v2: %w", errdefs.ErrNotImplemented)
	}
	m, err := cgroup2.Load(strings.TrimPrefix(path, "/sys/fs/cgroup"))
	if err != nil {
		return nil, err
	}
	return m.Stat()
}

// Update applies new resource limits (CRI UpdateContainerResources).
// Device rules are kept: they are not resources a pod resize changes.
func (c *Container) Update(res *specs.LinuxResources) error {
	c.op.Lock()
	defer c.op.Unlock()
	spec := *c.spec
	linux := *spec.Linux
	linux.Resources = res
	spec.Linux = &linux
	cg, err := specconv.CreateCgroupConfig(&specconv.CreateOpts{CgroupName: c.ID, Spec: &spec, UseSystemdCgroup: systemdCgroupPath(&spec)}, nil)
	if err != nil {
		return fmt.Errorf("convert resources: %w", err)
	}
	cfg := c.lc.Config()
	cg.Resources.Devices = cfg.Cgroups.Resources.Devices
	cfg.Cgroups.Resources = cg.Resources
	if err := c.lc.Set(cfg); err != nil {
		return err
	}
	// Keep later rewinds and restores on the new limits.
	c.cfg.Cgroups.Resources = cg.Resources
	c.spec = &spec
	return nil
}
