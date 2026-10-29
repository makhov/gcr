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
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	// Registers the cgroup device-rules driver libcontainer needs.
	_ "github.com/opencontainers/cgroups/devices"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/specconv"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/gpu"
	"github.com/makhov/gcr/internal/userns"
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

// CRIUConfig is a CRIU configuration file, which the container rewrites
// before each checkpoint and restore: libcontainer talks to CRIU over RPC,
// which has no field for many options, and names the file in the
// org.criu.config label.
type CRIUConfig struct {
	// Path is the file; "" keeps libcontainer's defaults.
	Path string
	// Options are config lines for every checkpoint and restore,
	// RestoreOptions for restores only.
	Options, RestoreOptions []string
}

// write writes the configuration for a checkpoint, or a restore.
func (cc CRIUConfig) write(restore bool) error {
	if cc.Path == "" {
		return nil
	}
	lines := slices.Clone(cc.Options)
	if restore {
		lines = append(lines, cc.RestoreOptions...)
	}
	if err := os.MkdirAll(filepath.Dir(cc.Path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(cc.Path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// Prepared is what PrepareSpec decided.
type Prepared struct {
	// CRIU configures this container's checkpoints and restores.
	CRIU CRIUConfig
	// GPU describes the container's GPUs, nil without; GPUError, why they
	// could not be described (checkpoints are refused then). Checkpoints
	// record the GPUs; restores check them against these.
	GPU      *gpu.Info
	GPUError error
	// CheckpointFrom, when set, makes Start wait for it to return a
	// directory of CRIU images and restore from it (incoming migration).
	CheckpointFrom func(context.Context) (string, error)
	// IDMap, when set (containerd's "0:host:size" form), mounts the image
	// layers idmapped for the container's user namespace and hands a fresh
	// writable layer to the mapped root (IDMapRoot).
	IDMap     string
	IDMapRoot int
	// IDKey is the allocator key that holds the range; snapshots record it
	// so a fork can map the same host IDs.
	IDKey string
	// IOOwner, if not 0, owns the stdio pipes when the container has a user
	// namespace gcr did not make (Kubernetes hostUsers: false).
	IOOwner int
	// LazyPages fills the restored memory on demand (AnnotationLazyRestore).
	LazyPages bool
	// MemoryMerge lets the kernel merge the container's memory with
	// identical memory elsewhere (AnnotationMemoryMerge): its init turns it
	// on for a new container, criu's process for a restored one.
	MemoryMerge bool
	// ForkFrom, when set, is a snapshot directory (Snapshot's output) this
	// container starts from: its processes are restored from the
	// snapshot's images, and its writable layer is the snapshot's, which
	// stays read-only underneath a fresh one (Layers). Forks of one
	// snapshot therefore cost one empty layer each, and none changes it.
	ForkFrom string
	// ForkSignal, if not 0, is sent to a fork's init once it is restored
	// (AnnotationForkSignal).
	ForkSignal unix.Signal
	// Layers are read-only layers mounted between the writable layer and
	// the image's, top first (ForkLayers, or a checkpoint's ReadLayers).
	Layers []string
	// CgroupOwner, if not 0, is given the container's cgroup (the host UID
	// of its root), so it can manage nested containers' cgroups.
	CgroupOwner int
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

	namespace      string
	cfg            *configs.Config
	rootfs         []mount.Mount
	layers         []string // read-only, between rootfs's upper and lower layers
	spec           *specs.Spec
	checkpoint     string
	checkpointFrom func(context.Context) (string, error)
	io             *stdio // pipes, when the init has no terminal
	tty            *ttyIO // when it has one

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
	// idBase is the first host ID of the user namespace range, 0 if none,
	// and idKey the allocator key that holds it.
	idBase uint32
	idKey  string
	// ioOwner owns the container's ends of the stdio pipes, 0 for root.
	ioOwner int
	// cgroupOwner, if not 0, is given the container's cgroup.
	cgroupOwner int
	// lazyPages restores memory on demand, through a criu lazy-pages
	// daemon this container starts for each restore.
	lazyPages bool
	// memoryMerge runs criu with memory merging on, which the tasks it
	// restores inherit.
	memoryMerge bool
	// forkSignal is sent to the init once a fork is restored.
	forkSignal unix.Signal
	criu       CRIUConfig
	gpu        *gpu.Info
	gpuError   error
	execs      map[string]*Exec
}

// AnnotationLazyRestore makes a restore fill memory on demand instead of
// loading it first: the container runs as soon as its processes are in
// place, and a criu lazy-pages daemon serves each page the moment it is
// touched. A fork of a large snapshot starts in the time its first pages
// take, not its whole memory.
const AnnotationLazyRestore = "gcr.dev/lazy-restore"

// AnnotationMemoryMerge lets the kernel merge the container's memory with
// identical memory of other containers that allow it (KSM), copying a page
// again when one writes it. Forks of one snapshot start with identical
// copies of its memory, so ten of them come to little more than one.
const AnnotationMemoryMerge = "gcr.dev/memory-merge"

// AnnotationForkFrom starts a container from a snapshot directory
// (Snapshot's output) instead of its image's entrypoint: its processes are
// restored from the snapshot and its writable layer sits on top of the
// snapshot's, which no fork changes.
const AnnotationForkFrom = "gcr.dev/fork-from"

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
	var prep Prepared
	if o.PrepareSpec != nil {
		if prep, err = o.PrepareSpec(spec); err != nil {
			return nil, err
		}
	}

	if prep.IDMap != "" {
		if o.Rootfs, err = idmapRootfs(o.Rootfs, prep.IDMap, prep.IDMapRoot); err != nil {
			return nil, err
		}
		// The container's init runs as the mapped root and must reach
		// bundle/rootfs; containerd creates the bundle 0700.
		if err := os.Chmod(o.Bundle, 0o711); err != nil {
			return nil, err
		}
	}
	if prep.ForkFrom != "" {
		o.Checkpoint = filepath.Join(prep.ForkFrom, "images")
		// After idmapRootfs, which gives an empty writable layer to the
		// container's root.
		info := ForkInfo{ID: o.ID, Snapshot: prep.ForkFrom, Created: time.Now().UTC()}
		if err := writeForkInfo(o.Rootfs, info, prep.IDMapRoot); err != nil {
			return nil, fmt.Errorf("fork: %w", err)
		}
	}
	rootfs := filepath.Join(o.Bundle, "rootfs")
	if len(o.Rootfs) > 0 {
		if err := os.MkdirAll(rootfs, 0o711); err != nil {
			return nil, err
		}
		if err := mountRootfs(o.Rootfs, rootfs, prep.Layers); err != nil {
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
		// "slice:prefix:name" is how CRI asks for the systemd cgroup driver.
		UseSystemdCgroup: systemdCgroupPath(spec),
	})
	if err != nil {
		return nil, fmt.Errorf("convert spec: %w", err)
	}
	if spec.Process.Terminal && (o.Checkpoint != "" || prep.CheckpointFrom != nil) {
		// CRIU restores only pipes for stdio; a pty would need --shell-job.
		return nil, fmt.Errorf("restore and migration need a container without a terminal: %w", errdefs.ErrNotImplemented)
	}
	if prep.CRIU.Path != "" {
		cfg.Labels = append(cfg.Labels, "org.criu.config="+prep.CRIU.Path)
		if err := prep.CRIU.write(false); err != nil {
			return nil, fmt.Errorf("CRIU config: %w", err)
		}
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

	var (
		io  *stdio
		tty *ttyIO
	)
	if spec.Process.Terminal {
		tty, err = openTTY(ctx, o.Stdin, o.Stdout)
	} else {
		io, err = openStdio(ctx, o.Stdin, o.Stdout, o.Stderr)
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			if tty != nil {
				tty.Close()
			} else {
				io.Close()
			}
		}
	}()

	c := &Container{
		ID:             o.ID,
		Bundle:         o.Bundle,
		namespace:      o.Namespace,
		cfg:            cfg,
		rootfs:         o.Rootfs,
		layers:         prep.Layers,
		lc:             lc,
		spec:           spec,
		checkpoint:     o.Checkpoint,
		checkpointFrom: prep.CheckpointFrom,
		io:             io,
		tty:            tty,
		idBase:         uint32(prep.IDMapRoot),
		idKey:          prep.IDKey,
		ioOwner:        max(prep.IDMapRoot, prep.IOOwner),
		cgroupOwner:    prep.CgroupOwner,
		lazyPages:      prep.LazyPages,
		memoryMerge:    prep.MemoryMerge,
		forkSignal:     prep.ForkSignal,
		criu:           prep.CRIU,
		gpu:            prep.GPU,
		gpuError:       prep.GPUError,
		status:         Created,
		exited:         make(chan struct{}),
		onExit:         onExit,
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
	if tty != nil {
		if err := tty.started(); err != nil {
			return nil, err
		}
	}
	if err := c.delegateCgroup(); err != nil {
		return nil, err
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
	if c.forkSignal != 0 {
		if err := c.lc.Signal(c.forkSignal); err != nil {
			return fmt.Errorf("fork signal: %w", err)
		}
	}
	return nil
}

// restore starts a new incarnation from CRIU images.
func (c *Container) restore(images string) error {
	saved, err := gpu.Load(images)
	if err != nil {
		return err
	}
	if err := gpu.Compatible(saved, c.gpu); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if err := c.criu.write(true); err != nil {
		return fmt.Errorf("CRIU config: %w", err)
	}
	defer c.criu.write(false)
	proc, err := c.initProcess()
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(c.Bundle, "criu-restore-")
	if err != nil {
		return err
	}
	if c.lazyPages {
		// Must be serving before the restore asks it for a page, and it
		// keeps going afterwards: the container faults pages in as it
		// runs, and the daemon exits once it has sent the last one.
		stop, err := startLazyPages(images, work)
		if err != nil {
			return err
		}
		defer stop()
	}
	err = c.lc.Restore(proc, &libcontainer.CriuOpts{
		ImagesDirectory: images,
		WorkDirectory:   work,
		LazyPages:       c.lazyPages,
		StatusFd:        -1,
		Command:         c.criuCommand(),
	})
	if err != nil {
		c.io.detach() // restore requires pipes, never a terminal
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
	if err := c.delegateCgroup(); err != nil {
		return err
	}
	return c.track(proc)
}

// Checkpoint dumps the container with CRIU into opts.ImagePath.
func (c *Container) Checkpoint(ctx context.Context, opts CheckpointOpts) error {
	c.op.Lock()
	defer c.op.Unlock()
	if c.gpuError != nil {
		return fmt.Errorf("checkpoint: %w", c.gpuError)
	}
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
	if err := c.saveGPU(opts.ImagePath); err != nil {
		return err
	}
	// A restore must mount the same layers under the writable layer
	// containerd saves with the checkpoint, which holds only what this
	// container changed above them.
	if err := writeLayers(opts.ImagePath, c.layers); err != nil {
		return err
	}
	// A restore must use the same user namespace range: CRIU restores the
	// processes into the mapping they were dumped with.
	if c.idBase != 0 {
		return os.WriteFile(filepath.Join(opts.ImagePath, userns.BaseFile), []byte(strconv.Itoa(int(c.idBase))), 0o600)
	}
	return nil
}

// IDBase is the container's user namespace range (first host ID), 0 if none.
func (c *Container) IDBase() uint32 { return c.idBase }

// Layers are the read-only layers between the container's writable layer
// and its image's, top first: those of the snapshots it was forked from.
func (c *Container) Layers() []string { return c.layers }

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
	if err := c.saveRange(dir); err != nil {
		return err
	}
	// dir/upper is only what this container changed above its layers;
	// forks of dir mount them beneath it.
	if err := writeLayers(dir, c.layers); err != nil {
		return err
	}
	return c.saveGPU(filepath.Join(dir, "images"))
}

// saveRange records the user namespace range the container maps, which a
// fork of this snapshot has to map as well: the writable layer the
// snapshot copied holds host IDs from it.
func (c *Container) saveRange(dir string) error {
	if c.idBase == 0 {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, userns.BaseFile), []byte(strconv.Itoa(int(c.idBase))), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, userns.KeyFile), []byte(c.idKey), 0o600)
}

// saveGPU records the container's GPUs with a checkpoint's images.
func (c *Container) saveGPU(images string) error {
	if c.gpu == nil {
		return nil
	}
	return c.gpu.Save(images)
}

// CanPreDump says whether pre-copy rounds can run: CRIU's CUDA plugin
// cannot pre-dump, so containers with GPUs migrate stop-and-copy.
func (c *Container) CanPreDump() bool { return c.gpu == nil && c.gpuError == nil }

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
	// The snapshot's writable layer is what changed above its layers,
	// and only means the same above the same ones.
	layers, err := ReadLayers(dir)
	if err != nil {
		return err
	}
	if !slices.Equal(layers, c.layers) {
		return fmt.Errorf("%s was taken above other layers (%q) than this container has (%q): %w",
			dir, layers, c.layers, errdefs.ErrFailedPrecondition)
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
	if err := mountRootfs(c.rootfs, rootfs, c.layers); err != nil {
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
	if !c.CanPreDump() {
		return fmt.Errorf("pre-dump of a container with GPUs: %w", errdefs.ErrNotImplemented)
	}
	return c.dump(dir, parent, host, port, true)
}

// FreezeDump freezes the container and dumps it to a page server and dir.
// The container stays frozen: Thaw on failure, Stop once the target runs.
func (c *Container) FreezeDump(ctx context.Context, dir, parent, host string, port int) error {
	c.op.Lock()
	defer c.op.Unlock()
	if c.gpuError != nil {
		return fmt.Errorf("checkpoint: %w", c.gpuError)
	}
	if err := c.lc.Pause(); err != nil {
		return fmt.Errorf("freeze: %w", err)
	}
	if err := c.dump(dir, parent, host, port, false); err != nil {
		_ = c.lc.Resume()
		return err
	}
	if err := c.saveGPU(dir); err != nil {
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

// LazyPagesAvailable says whether a container in a user namespace can be
// restored lazily. The restored tasks open the userfaultfd themselves, from
// inside the namespace, and the kernel allows that unrestricted only to
// CAP_SYS_PTRACE in the initial user namespace or when
// vm.unprivileged_userfaultfd is 1. The restricted form
// (UFFD_USER_MODE_ONLY) is not enough: the kernel also faults a restored
// process's pages on its behalf, during a read into one for instance, and
// those faults would reach nobody.
func LazyPagesAvailable() bool {
	b, err := os.ReadFile("/proc/sys/vm/unprivileged_userfaultfd")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// startLazyPages runs a criu lazy-pages daemon that serves the images'
// pages to a restore, and returns a function that stops waiting on it.
// Both sides find each other through a socket named relative to the work
// directory, so they must share one. The returned function does not kill
// the daemon: it has to outlive the restore, until the container has
// touched every page it needs.
func startLazyPages(images, work string) (func(), error) {
	ready, signal, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer signal.Close()
	cmd := exec.Command("criu", "lazy-pages", "--images-dir", images, "--work-dir", work, "--status-fd", "3")
	cmd.ExtraFiles = []*os.File{signal}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		ready.Close()
		return nil, fmt.Errorf("lazy pages: %w", err)
	}
	// It writes one byte once it is listening; a closed pipe means it
	// died first, and its log says why.
	_ = ready.SetReadDeadline(time.Now().Add(30 * time.Second))
	var b [1]byte
	if _, err := ready.Read(b[:]); err != nil {
		ready.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("lazy pages: not serving: %w%s", err, criuErrors(filepath.Join(work, "lazy-pages.log")))
	}
	ready.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := cmd.Wait(); err != nil {
			// The container is left with pages nobody will send it; it
			// gets SIGBUS when it touches one.
			log.G(context.Background()).WithError(err).
				WithField("log", filepath.Join(work, "lazy-pages.log")).Error("lazy pages daemon stopped early")
		}
	}()
	return func() {
		select {
		case <-done:
		default:
		}
	}, nil
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

// mountAll mounts a copy of mounts: containerd's idmapped overlay setup
// rewrites the options slice in place (lowerdir= to temporary idmapped
// copies it removes afterwards), and the originals are mounted again on a
// rewind.
func mountAll(mounts []mount.Mount, target string) error {
	cp := make([]mount.Mount, len(mounts))
	for i, m := range mounts {
		m.Options = slices.Clone(m.Options)
		cp[i] = m
	}
	return mount.All(cp, target)
}

func systemdCgroupPath(spec *specs.Spec) bool {
	return spec.Linux != nil && strings.Count(spec.Linux.CgroupsPath, ":") == 2
}

// idmapRootfs adds idmapping to an overlay rootfs: containerd's mount code
// idmaps the lower layers, and a still empty upper layer is given to the
// container's root so files it creates are owned by the mapped IDs.
func idmapRootfs(mounts []mount.Mount, idmap string, root int) ([]mount.Mount, error) {
	out := slices.Clone(mounts)
	for i, m := range out {
		if m.Type != "overlay" {
			return nil, fmt.Errorf("user namespaces need an overlay rootfs, got %q: %w", m.Type, errdefs.ErrNotImplemented)
		}
		for _, o := range m.Options {
			upper, ok := strings.CutPrefix(o, "upperdir=")
			if !ok {
				continue
			}
			entries, err := os.ReadDir(upper)
			if err != nil {
				return nil, err
			}
			if len(entries) == 0 {
				if err := os.Chown(upper, root, root); err != nil {
					return nil, fmt.Errorf("give writable layer to the container's root: %w", err)
				}
			}
		}
		out[i].Options = append(slices.Clone(m.Options), "uidmap="+idmap, "gidmap="+idmap)
	}
	return out, nil
}

// delegateCgroup gives the container's cgroup to cgroupOwner, the way
// cgroup v2 delegation is defined: the directory and the files that move
// processes and enable controllers.
// CgroupPath is the container's cgroup directory (cgroup v2).
func (c *Container) CgroupPath() (string, error) {
	st, err := c.lc.State()
	if err != nil {
		return "", err
	}
	dir, ok := st.CgroupPaths[""]
	if !ok {
		return "", fmt.Errorf("no unified cgroup: %w", errdefs.ErrNotImplemented)
	}
	return dir, nil
}

func (c *Container) delegateCgroup() error {
	if c.cgroupOwner == 0 {
		return nil
	}
	st, err := c.lc.State()
	if err != nil {
		return err
	}
	dir, ok := st.CgroupPaths[""]
	if !ok {
		return fmt.Errorf("cgroup delegation needs cgroup v2: %w", errdefs.ErrNotImplemented)
	}
	for _, p := range []string{dir, filepath.Join(dir, "cgroup.procs"), filepath.Join(dir, "cgroup.threads"), filepath.Join(dir, "cgroup.subtree_control")} {
		if err := os.Lchown(p, c.cgroupOwner, c.cgroupOwner); err != nil {
			return fmt.Errorf("delegate cgroup: %w", err)
		}
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
	c.mu.Lock()
	execs := c.execs
	c.execs = nil
	c.mu.Unlock()
	for _, e := range execs {
		e.closeIO()
	}
	if c.tty != nil {
		c.tty.Close()
	} else {
		c.io.Close()
	}
	return errors.Join(errs...)
}

// Resize sets the init's terminal size.
func (c *Container) Resize(width, height uint32) error {
	if c.tty == nil {
		return fmt.Errorf("container has no terminal: %w", errdefs.ErrFailedPrecondition)
	}
	return c.tty.resize(width, height)
}

// CloseStdin gives the init EOF on stdin.
func (c *Container) CloseStdin() {
	if c.tty != nil {
		c.tty.closeStdin()
	} else {
		c.io.closeStdin()
	}
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
	if c.tty != nil {
		c.tty.setup(proc, c.spec.Process.ConsoleSize)
		return proc, nil
	}
	if proc.Stdin, proc.Stdout, proc.Stderr, err = c.io.attach(c.ioOwner); err != nil {
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
	if c.io != nil {
		c.io.detach()
	}

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
		if c.tty != nil {
			c.tty.wait()
			c.tty.Close()
		} else {
			c.io.wait()
			// containerd (and ctr attached to the task) read the fifos
			// until EOF.
			c.io.Close()
		}
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
