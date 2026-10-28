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
	"strings"
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
	// PrepareSpec may change the spec before it is converted, and decides
	// the rest of the container's setup from it.
	PrepareSpec func(*specs.Spec) (Prepared, error)
}

// Prepared is what PrepareSpec decided.
type Prepared struct {
	// CRIUConfig is a CRIU configuration file for this container's
	// checkpoints and restores; "" keeps libcontainer's default.
	CRIUConfig string
	// CheckpointFrom, when set, makes Start wait for it to return a
	// directory of CRIU images and restore from it (incoming migration).
	CheckpointFrom func(context.Context) (string, error)
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

	namespace  string
	cfg        *configs.Config
	rootfs     []mount.Mount
	spec           *specs.Spec
	checkpoint     string
	checkpointFrom func(context.Context) (string, error)
	io             *stdio

	// op serializes operations that replace or freeze the processes
	// (snapshot, rewind) against each other and against delete.
	op sync.Mutex
	lc *libcontainer.Container

	mu         sync.Mutex
	status     Status
	pid        int
	exitStatus uint32
	exitedAt   time.Time
	exited     chan struct{}
	onExit     func(*Container)
	// replacing is set while a rewind kills the processes; their exit
	// is then reported on replaced instead of to containerd.
	replacing bool
	replaced  chan struct{}
	// migrating blocks snapshots and rewinds while a migration dumps.
	migrating bool
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
	var prep Prepared
	if o.PrepareSpec != nil {
		if prep, err = o.PrepareSpec(spec); err != nil {
			return nil, err
		}
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
	if prep.CRIUConfig != "" {
		cfg.Labels = append(cfg.Labels, "org.criu.config="+prep.CRIUConfig)
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
		namespace:  o.Namespace,
		cfg:        cfg,
		rootfs:     o.Rootfs,
		lc:         lc,
		spec:       spec,
		checkpoint:     o.Checkpoint,
		checkpointFrom: prep.CheckpointFrom,
		io:             io,
		status:     Created,
		exited:     make(chan struct{}),
		onExit:     onExit,
	}
	if o.Checkpoint != "" || prep.CheckpointFrom != nil {
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
	if c.checkpointFrom != nil {
		dir, err := c.checkpointFrom(ctx)
		if err != nil {
			return err
		}
		c.checkpoint = dir
	}
	if c.checkpoint == "" {
		if err := c.lc.Exec(); err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		c.setStatus(Running)
		return nil
	}
	if err := c.restore(c.checkpoint); err != nil {
		return err
	}
	c.setStatus(Running)
	return nil
}

// restore starts a new incarnation from CRIU images.
func (c *Container) restore(images string) error {
	proc, err := c.initProcess()
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(c.Bundle, "criu-restore-")
	if err != nil {
		return err
	}
	err = c.lc.Restore(proc, &libcontainer.CriuOpts{
		ImagesDirectory: images,
		WorkDirectory:   work,
	})
	if err != nil {
		c.io.detach()
		return fmt.Errorf("restore (criu log in %s): %w", work, err)
	}
	_ = os.RemoveAll(work)
	// libcontainer leaves a restored container in a state that refuses
	// pause and checkpoint; loading it from disk gives the running state,
	// as a later runc command would see it.
	lc, err := libcontainer.Load(Root(c.namespace), c.ID)
	if err != nil {
		return fmt.Errorf("reload restored container: %w", err)
	}
	c.lc = lc
	return c.track(proc)
}

// Checkpoint dumps the container with CRIU into opts.ImagePath.
func (c *Container) Checkpoint(ctx context.Context, opts CheckpointOpts) error {
	c.op.Lock()
	defer c.op.Unlock()
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

// Running reports whether the container's processes have started.
func (c *Container) Running() bool {
	return c.Status() == Running
}

// Snapshot freezes the container, copies its writable layer to dir/upper and
// dumps its processes to dir/images, then lets it continue. Freezing first
// keeps the files and the memory image consistent with each other.
func (c *Container) Snapshot(ctx context.Context, dir string) (retErr error) {
	c.op.Lock()
	defer c.op.Unlock()
	if !c.Running() || c.isMigrating() {
		return fmt.Errorf("container is not running or is migrating: %w", errdefs.ErrFailedPrecondition)
	}
	upper, err := c.UpperDir()
	if err != nil {
		return err
	}
	if err := c.lc.Pause(); err != nil {
		return fmt.Errorf("freeze: %w", err)
	}
	defer func() {
		if err := c.lc.Resume(); err != nil && retErr == nil {
			retErr = fmt.Errorf("thaw: %w", err)
		}
	}()
	if err := copyTree(upper, filepath.Join(dir, "upper")); err != nil {
		return fmt.Errorf("copy writable layer: %w", err)
	}
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	err = c.lc.Checkpoint(&libcontainer.CriuOpts{
		ImagesDirectory: filepath.Join(dir, "images"),
		WorkDirectory:   work,
		LeaveRunning:    true,
	})
	if err != nil {
		return fmt.Errorf("checkpoint (criu log in %s): %w", work, err)
	}
	return nil
}

// Rewind replaces the running processes and writable layer with the ones in
// dir (written by Snapshot). containerd keeps seeing one running task.
func (c *Container) Rewind(ctx context.Context, dir string) error {
	c.op.Lock()
	defer c.op.Unlock()
	if !c.Running() || c.isMigrating() {
		return fmt.Errorf("container is not running or is migrating: %w", errdefs.ErrFailedPrecondition)
	}
	upper, err := c.UpperDir()
	if err != nil {
		return err
	}

	replaced := make(chan struct{})
	c.mu.Lock()
	c.replacing = true
	c.replaced = replaced
	c.mu.Unlock()
	if err := c.lc.Signal(unix.SIGKILL); err != nil && !errors.Is(err, libcontainer.ErrNotRunning) {
		c.mu.Lock()
		c.replacing = false
		c.mu.Unlock()
		return fmt.Errorf("stop processes: %w", err)
	}
	<-replaced

	// From here on the old processes are gone; a failure leaves the task
	// stopped, and containerd is told it exited.
	if err := c.replace(upper, dir); err != nil {
		c.mu.Lock()
		c.replacing = false
		c.mu.Unlock()
		c.markExited(nil)
		return err
	}
	c.mu.Lock()
	c.replacing = false
	c.mu.Unlock()
	return nil
}

func (c *Container) replace(upper, dir string) error {
	if err := c.lc.Destroy(); err != nil {
		return fmt.Errorf("destroy old container: %w", err)
	}
	rootfs := filepath.Join(c.Bundle, "rootfs")
	if err := mount.UnmountRecursive(rootfs, 0); err != nil {
		return fmt.Errorf("unmount rootfs: %w", err)
	}
	if err := emptyDir(upper); err != nil {
		return fmt.Errorf("clear writable layer: %w", err)
	}
	if err := copyTree(filepath.Join(dir, "upper"), upper); err != nil {
		return fmt.Errorf("restore writable layer: %w", err)
	}
	if err := mount.All(c.rootfs, rootfs); err != nil {
		return fmt.Errorf("mount rootfs: %w", err)
	}
	lc, err := libcontainer.Create(Root(c.namespace), c.ID, c.cfg)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	c.lc = lc
	return c.restore(filepath.Join(dir, "images"))
}

// SetMigrating marks the container as being migrated out (true) or not.
func (c *Container) SetMigrating(on bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if on && c.migrating {
		return fmt.Errorf("already migrating: %w", errdefs.ErrFailedPrecondition)
	}
	c.migrating = on
	return nil
}

func (c *Container) isMigrating() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.migrating
}

// PreDump sends memory dirtied since parent to a CRIU page server while
// the container keeps running (a pre-copy round).
func (c *Container) PreDump(ctx context.Context, dir, parent, host string, port int) error {
	c.op.Lock()
	defer c.op.Unlock()
	return c.dump(dir, parent, host, port, true)
}

// FreezeDump freezes the container and dumps it to a page server and dir.
// The container stays frozen: Thaw on failure, Stop once the target runs.
func (c *Container) FreezeDump(ctx context.Context, dir, parent, host string, port int) error {
	c.op.Lock()
	defer c.op.Unlock()
	if err := c.lc.Pause(); err != nil {
		return fmt.Errorf("freeze: %w", err)
	}
	if err := c.dump(dir, parent, host, port, false); err != nil {
		_ = c.lc.Resume()
		return err
	}
	return nil
}

func (c *Container) dump(dir, parent, host string, port int, pre bool) error {
	err := c.lc.Checkpoint(&libcontainer.CriuOpts{
		ImagesDirectory: dir,
		WorkDirectory:   dir,
		ParentImage:     parent,
		PreDump:         pre,
		LeaveRunning:    !pre,
		PageServer:      libcontainer.CriuPageServerInfo{Address: host, Port: int32(port)},
	})
	if err != nil {
		return fmt.Errorf("criu dump: %w%s", err, criuErrors(filepath.Join(dir, "dump.log")))
	}
	return nil
}

// criuErrors returns the error lines of a CRIU log, for error messages.
func criuErrors(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var errs []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "Error (") {
			errs = append(errs, strings.TrimSpace(l))
		}
	}
	if len(errs) > 5 {
		errs = errs[len(errs)-5:]
	}
	if len(errs) == 0 {
		return " (log: " + path + ")"
	}
	return ": " + strings.Join(errs, "; ")
}

// Thaw lets a container frozen by FreezeDump continue.
func (c *Container) Thaw() error {
	c.op.Lock()
	defer c.op.Unlock()
	return c.lc.Resume()
}

// Stop kills a container frozen by FreezeDump; containerd sees it exit.
func (c *Container) Stop() error {
	c.op.Lock()
	defer c.op.Unlock()
	if err := c.lc.Signal(unix.SIGKILL); err != nil && !errors.Is(err, libcontainer.ErrNotRunning) {
		return err
	}
	// Thaw so the killed processes can exit; they may already be gone.
	if err := c.lc.Resume(); err != nil && !errors.Is(err, libcontainer.ErrNotPaused) {
		return err
	}
	return nil
}

// UpperDir returns the writable layer of an overlay rootfs.
func (c *Container) UpperDir() (string, error) {
	for _, m := range c.rootfs {
		if m.Type != "overlay" {
			continue
		}
		for _, o := range m.Options {
			if after, ok := strings.CutPrefix(o, "upperdir="); ok {
				return after, nil
			}
		}
	}
	return "", fmt.Errorf("time travel needs an overlay rootfs with a writable layer: %w", errdefs.ErrNotImplemented)
}

// Kill signals the init process, or every process in the container.
func (c *Container) Kill(sig syscall.Signal, all bool) error {
	c.op.Lock()
	lc := c.lc
	c.op.Unlock()
	if !all {
		if err := lc.Signal(sig); err != nil {
			if errors.Is(err, libcontainer.ErrNotRunning) {
				return fmt.Errorf("container not running: %w", errdefs.ErrNotFound)
			}
			return err
		}
		return nil
	}
	pids, err := lc.Processes()
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
	c.op.Lock()
	defer c.op.Unlock()
	if s := c.Status(); s == Running || s == Paused {
		return fmt.Errorf("container is running: %w", errdefs.ErrFailedPrecondition)
	}
	if c.Status() == Created && c.Pid() != 0 {
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
	c.op.Lock()
	defer c.op.Unlock()
	if err := c.lc.Pause(); err != nil {
		return err
	}
	c.setStatus(Paused)
	return nil
}

func (c *Container) Resume() error {
	c.op.Lock()
	defer c.op.Unlock()
	if err := c.lc.Resume(); err != nil {
		return err
	}
	c.setStatus(Running)
	return nil
}

func (c *Container) Pids() ([]int, error) {
	c.op.Lock()
	defer c.op.Unlock()
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
	if proc.Stdin, proc.Stdout, proc.Stderr, err = c.io.attach(); err != nil {
		return nil, err
	}
	return proc, nil
}

// track records the init pid of a new incarnation and waits for it to exit
// in the background.
func (c *Container) track(proc *libcontainer.Process) error {
	pid, err := proc.Pid()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.pid = pid
	c.mu.Unlock()
	c.io.detach()

	go func() {
		st, _ := proc.Wait()
		c.mu.Lock()
		if c.replacing {
			// A rewind killed this incarnation; it is not an exit.
			ch := c.replaced
			c.mu.Unlock()
			close(ch)
			return
		}
		c.mu.Unlock()
		c.io.wait()
		c.markExited(st)
	}()
	return nil
}

// markExited records the init's exit and tells containerd.
func (c *Container) markExited(st *os.ProcessState) {
	c.mu.Lock()
	if c.status == Stopped {
		c.mu.Unlock()
		return
	}
	c.status = Stopped
	c.exitStatus = exitStatus(st)
	c.exitedAt = time.Now()
	c.mu.Unlock()
	close(c.exited)
	if c.onExit != nil {
		c.onExit(c)
	}
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
