//go:build linux

// Package container runs one OCI container in-process through libcontainer.
// It owns the rootfs mounts, the init process, its stdio and its exit status.
package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	// Registers the cgroup device-rules driver libcontainer needs.
	_ "github.com/opencontainers/cgroups/devices"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/specconv"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// StateRoot holds libcontainer state, one directory per containerd namespace.
const StateRoot = "/run/gcr"

// Status is the lifecycle state reported to containerd.
type Status int

const (
	Created Status = iota
	Running
	Paused
	Stopped
)

type CreateOpts struct {
	ID        string
	Namespace string
	Bundle    string
	Rootfs    []mount.Mount
	Stdin     string
	Stdout    string
	Stderr    string
	// Checkpoint is a directory of CRIU images; when set, Start restores
	// from it instead of running the spec's process.
	Checkpoint string
}

type CheckpointOpts struct {
	ImagePath   string
	WorkPath    string
	Exit        bool
	OpenTCP     bool
	UnixSockets bool
	FileLocks   bool
}

type Container struct {
	ID     string
	Bundle string

	lc         *libcontainer.Container
	spec       *specs.Spec
	checkpoint string
	io         *stdio

	mu         sync.Mutex
	status     Status
	pid        int
	exitStatus uint32
	exitedAt   time.Time
	exited     chan struct{}
	onExit     func(*Container)
}

// Root returns the libcontainer state directory for a containerd namespace.
func Root(namespace string) string {
	return filepath.Join(StateRoot, namespace)
}

// Create mounts the rootfs and creates the container. The process is set up
// and paused before exec; Start releases it (or restores, for a checkpoint).
func Create(ctx context.Context, o CreateOpts, onExit func(*Container)) (_ *Container, retErr error) {
	spec, err := loadSpec(o.Bundle)
	if err != nil {
		return nil, err
	}
	if spec.Process == nil {
		return nil, fmt.Errorf("spec has no process: %w", errdefs.ErrInvalidArgument)
	}
	if spec.Process.Terminal {
		return nil, fmt.Errorf("terminal: %w", errdefs.ErrNotImplemented)
	}

	rootfs := filepath.Join(o.Bundle, "rootfs")
	if len(o.Rootfs) > 0 {
		if err := os.MkdirAll(rootfs, 0o711); err != nil {
			return nil, err
		}
		if err := mount.All(o.Rootfs, rootfs); err != nil {
			return nil, fmt.Errorf("mount rootfs: %w", err)
		}
		defer func() {
			if retErr != nil {
				_ = mount.UnmountRecursive(rootfs, 0)
			}
		}()
	}
	if !filepath.IsAbs(spec.Root.Path) {
		spec.Root.Path = filepath.Join(o.Bundle, spec.Root.Path)
	}

	cfg, err := specconv.CreateLibcontainerConfig(&specconv.CreateOpts{
		CgroupName: o.ID,
		Spec:       spec,
	})
	if err != nil {
		return nil, fmt.Errorf("convert spec: %w", err)
	}
	lc, err := libcontainer.Create(Root(o.Namespace), o.ID, cfg)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = lc.Destroy()
		}
	}()

	io, err := openStdio(ctx, o.Stdin, o.Stdout, o.Stderr)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			io.Close()
		}
	}()

	c := &Container{
		ID:         o.ID,
		Bundle:     o.Bundle,
		lc:         lc,
		spec:       spec,
		checkpoint: o.Checkpoint,
		io:         io,
		status:     Created,
		exited:     make(chan struct{}),
		onExit:     onExit,
	}
	if o.Checkpoint != "" {
		// Restore happens in Start, the way containerd expects.
		return c, nil
	}

	proc, err := c.initProcess()
	if err != nil {
		return nil, err
	}
	if err := lc.Start(proc); err != nil {
		return nil, fmt.Errorf("start init: %w", err)
	}
	if err := c.track(proc); err != nil {
		return nil, err
	}
	return c, nil
}

// Start runs the created container, or restores it from its checkpoint.
func (c *Container) Start(ctx context.Context) error {
	if c.checkpoint == "" {
		if err := c.lc.Exec(); err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		c.setStatus(Running)
		return nil
	}

	proc, err := c.initProcess()
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(c.Bundle, "criu-restore-")
	if err != nil {
		return err
	}
	err = c.lc.Restore(proc, &libcontainer.CriuOpts{
		ImagesDirectory: c.checkpoint,
		WorkDirectory:   work,
	})
	if err != nil {
		return fmt.Errorf("restore (criu log in %s): %w", work, err)
	}
	if err := c.track(proc); err != nil {
		return err
	}
	c.setStatus(Running)
	return nil
}

// Checkpoint dumps the container with CRIU into opts.ImagePath.
func (c *Container) Checkpoint(ctx context.Context, opts CheckpointOpts) error {
	work := opts.WorkPath
	if work == "" {
		var err error
		if work, err = os.MkdirTemp(c.Bundle, "criu-dump-"); err != nil {
			return err
		}
	}
	err := c.lc.Checkpoint(&libcontainer.CriuOpts{
		ImagesDirectory:         opts.ImagePath,
		WorkDirectory:           work,
		LeaveRunning:            !opts.Exit,
		TcpEstablished:          opts.OpenTCP,
		ExternalUnixConnections: opts.UnixSockets,
		FileLocks:               opts.FileLocks,
	})
	if err != nil {
		return fmt.Errorf("checkpoint (criu log in %s): %w", work, err)
	}
	return nil
}

// Kill signals the init process, or every process in the container.
func (c *Container) Kill(sig syscall.Signal, all bool) error {
	if !all {
		if err := c.lc.Signal(sig); err != nil {
			if errors.Is(err, libcontainer.ErrNotRunning) {
				return fmt.Errorf("container not running: %w", errdefs.ErrNotFound)
			}
			return err
		}
		return nil
	}
	pids, err := c.lc.Processes()
	if err != nil {
		return err
	}
	for _, pid := range pids {
		if err := unix.Kill(pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
	}
	return nil
}

// Delete removes a stopped container: libcontainer state, rootfs mounts, stdio.
func (c *Container) Delete(ctx context.Context) error {
	if c.Status() == Running {
		return fmt.Errorf("container is running: %w", errdefs.ErrFailedPrecondition)
	}
	if c.Status() == Created && c.pid != 0 {
		// Created but never started: the init is parked before exec.
		_ = c.lc.Signal(unix.SIGKILL)
		<-c.exited
	}
	var errs []error
	if err := c.lc.Destroy(); err != nil {
		errs = append(errs, err)
	}
	if err := mount.UnmountRecursive(filepath.Join(c.Bundle, "rootfs"), 0); err != nil {
		errs = append(errs, err)
	}
	c.io.Close()
	return errors.Join(errs...)
}

// Pause freezes every process in the container's cgroup.
func (c *Container) Pause() error {
	if err := c.lc.Pause(); err != nil {
		return err
	}
	c.setStatus(Paused)
	return nil
}

func (c *Container) Resume() error {
	if err := c.lc.Resume(); err != nil {
		return err
	}
	c.setStatus(Running)
	return nil
}

func (c *Container) Pids() ([]int, error) {
	return c.lc.Processes()
}

func (c *Container) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Container) Pid() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pid
}

// Exit returns the exit status and time once the init has exited.
func (c *Container) Exit() (uint32, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitStatus, c.exitedAt
}

// Exited is closed when the init process exits.
func (c *Container) Exited() <-chan struct{} {
	return c.exited
}

func (c *Container) setStatus(s Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status != Stopped {
		c.status = s
	}
}

func (c *Container) initProcess() (*libcontainer.Process, error) {
	proc, err := newProcess(c.spec.Process)
	if err != nil {
		return nil, err
	}
	proc.Init = true
	proc.Stdin, proc.Stdout, proc.Stderr = c.io.containerSide()
	return proc, nil
}

// track records the init pid and waits for it to exit in the background.
func (c *Container) track(proc *libcontainer.Process) error {
	pid, err := proc.Pid()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.pid = pid
	c.mu.Unlock()
	c.io.closeContainerSide()

	go func() {
		st, _ := proc.Wait()
		c.mu.Lock()
		c.status = Stopped
		c.exitStatus = exitStatus(st)
		c.exitedAt = time.Now()
		c.mu.Unlock()
		c.io.wait()
		close(c.exited)
		if c.onExit != nil {
			c.onExit(c)
		}
	}()
	return nil
}

func exitStatus(st *os.ProcessState) uint32 {
	if st == nil {
		return 255
	}
	ws, ok := st.Sys().(syscall.WaitStatus)
	if !ok {
		return uint32(st.ExitCode())
	}
	if ws.Signaled() {
		return 128 + uint32(ws.Signal())
	}
	return uint32(ws.ExitStatus())
}

func loadSpec(bundle string) (*specs.Spec, error) {
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, err
	}
	var s specs.Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse config.json: %w", err)
	}
	if s.Root == nil {
		return nil, fmt.Errorf("spec has no root: %w", errdefs.ErrInvalidArgument)
	}
	return &s, nil
}

// newProcess converts the spec's process, as runc does.
func newProcess(p *specs.Process) (*libcontainer.Process, error) {
	lp := &libcontainer.Process{
		Args:            p.Args,
		Env:             p.Env,
		UID:             int(p.User.UID),
		GID:             int(p.User.GID),
		Cwd:             p.Cwd,
		Label:           p.SelinuxLabel,
		NoNewPrivileges: &p.NoNewPrivileges,
		AppArmorProfile: p.ApparmorProfile,
		Scheduler:       p.Scheduler,
		IOPriority:      p.IOPriority,
	}
	if p.Capabilities != nil {
		lp.Capabilities = &configs.Capabilities{
			Bounding:    p.Capabilities.Bounding,
			Effective:   p.Capabilities.Effective,
			Inheritable: p.Capabilities.Inheritable,
			Permitted:   p.Capabilities.Permitted,
			Ambient:     p.Capabilities.Ambient,
		}
	}
	for _, g := range p.User.AdditionalGids {
		lp.AdditionalGroups = append(lp.AdditionalGroups, int(g))
	}
	for _, r := range p.Rlimits {
		t, ok := rlimits[r.Type]
		if !ok {
			return nil, fmt.Errorf("unknown rlimit %q: %w", r.Type, errdefs.ErrInvalidArgument)
		}
		lp.Rlimits = append(lp.Rlimits, configs.Rlimit{Type: t, Hard: r.Hard, Soft: r.Soft})
	}
	aff, err := configs.ConvertCPUAffinity(p.ExecCPUAffinity)
	if err != nil {
		return nil, err
	}
	lp.CPUAffinity = aff
	return lp, nil
}

var rlimits = map[string]int{
	"RLIMIT_AS":         unix.RLIMIT_AS,
	"RLIMIT_CORE":       unix.RLIMIT_CORE,
	"RLIMIT_CPU":        unix.RLIMIT_CPU,
	"RLIMIT_DATA":       unix.RLIMIT_DATA,
	"RLIMIT_FSIZE":      unix.RLIMIT_FSIZE,
	"RLIMIT_LOCKS":      unix.RLIMIT_LOCKS,
	"RLIMIT_MEMLOCK":    unix.RLIMIT_MEMLOCK,
	"RLIMIT_MSGQUEUE":   unix.RLIMIT_MSGQUEUE,
	"RLIMIT_NICE":       unix.RLIMIT_NICE,
	"RLIMIT_NOFILE":     unix.RLIMIT_NOFILE,
	"RLIMIT_NPROC":      unix.RLIMIT_NPROC,
	"RLIMIT_RSS":        unix.RLIMIT_RSS,
	"RLIMIT_RTPRIO":     unix.RLIMIT_RTPRIO,
	"RLIMIT_RTTIME":     unix.RLIMIT_RTTIME,
	"RLIMIT_SIGPENDING": unix.RLIMIT_SIGPENDING,
	"RLIMIT_STACK":      unix.RLIMIT_STACK,
}
