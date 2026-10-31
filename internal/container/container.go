//go:build linux

// Package container runs one OCI container in-process through libcontainer.
// It owns the rootfs mounts, the init process, its stdio and its exit status.
package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/makhov/gcr/internal/ocispec"
	"github.com/makhov/gcr/internal/snapshot"
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
	// OnOOM is called when the OOM killer has killed one of the
	// container's processes.
	OnOOM func(*Container)
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
	CheckpointFrom func(context.Context) (Incoming, error)
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
	// TrackMemory makes every dump track the pages written after it, so
	// a Point can dump only those (time travel, where the kernel can).
	TrackMemory bool
	// Rewinds: the container's processes may be replaced in place (time
	// travel).
	Rewinds bool
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

	namespace string
	cfg       *configs.Config
	rootfs    []mount.Mount
	layers    []string // read-only, between rootfs's upper and lower layers
	// rootfsDir is where rootfs is mounted: the bundle's rootfs, or where
	// the caller mounted it (callerMounted; CRI-O and Docker do).
	rootfsDir     string
	callerMounted bool
	// callerMounts is the caller's own mount, put back on delete where gcr
	// mounted another over it.
	callerMounts []mount.Mount
	// callerPath, if set, is where the caller mounted the rootfs, which gcr
	// mounted at rootfsDir instead and binds there.
	callerPath string
	// frozen: restored when created and frozen until Start (CRI-O).
	frozen         bool
	snapshots      *snapshot.Hold
	spec           *specs.Spec
	checkpoint     string
	checkpointFrom func(context.Context) (Incoming, error)
	// remotePages is the page server a post-copy migration's memory comes
	// from, as it is touched and then in the background.
	remotePages string
	io          *stdio // pipes, when the init has no terminal
	tty         *ttyIO // when it has one

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
	onOOM      func(*Container)
	oomKills   uint64 // told of by onOOM; guarded by mu
	cgroupDir  string // read at start, for OOM kills at exit; guarded by mu
	oomInvoked uint64 // the OOM killer's invocations seen; guarded by mu
	// parentKills is the parent cgroup's OOM kills when the watch began.
	parentKills uint64
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
	// forkImages are the images of the snapshot a fork restores, which
	// stay while it runs (it holds the snapshot): their lazy-pages daemon
	// serves only the pages the fork touches, for as long as it runs.
	forkImages string
	// lazyDaemon fills the container's memory, until it has (or, on
	// demand, for as long as the container runs); nil once it has.
	lazyDaemon *lazyDaemon
	// memoryMerge runs criu with memory merging on, which the tasks it
	// restores inherit.
	memoryMerge bool
	// tracking: dumps track memory (TrackMemory); trackedSince is the
	// point the tracked pages were written after, "" if none. Guarded
	// by op.
	tracking     bool
	trackedSince string
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

	// CRI-O and Docker mount the rootfs themselves and pass none. gcr then
	// works with the overlay they mounted: its writable layer, and the same
	// overlay mounted again where it needs another, with a fork's snapshot
	// layers or with its image layers idmapped.
	rootfsDir := filepath.Join(o.Bundle, "rootfs")
	callerMounted := false
	var callerMounts []mount.Mount // as the caller mounted it
	if len(o.Rootfs) == 0 && spec.Root != nil && filepath.IsAbs(spec.Root.Path) {
		if m := callerRootfs(spec.Root.Path); m != nil {
			if prep.IDMap != "" && ocispec.CRIO(spec) {
				// Not tried yet; pods have CRI-O's own (hostUsers: false).
				return nil, errRootfs("gcr's user namespaces")
			}
			if prep.Rewinds && ocispec.CRIO(spec) {
				// CRI-O knows the container by its init's pid and start
				// time, and a rewind brings another: CRI-O would take
				// the container for gone.
				return nil, fmt.Errorf("time travel in a container CRI-O runs: a rewind replaces the process "+
					"CRI-O tracks the container by: %w", errdefs.ErrNotImplemented)
			}
			o.Rootfs, rootfsDir, callerMounted = m, spec.Root.Path, true
			callerMounts = m
		}
	}

	// The snapshots the layers are from must not go while the container
	// mounts them.
	hold, err := snapshot.Acquire(o.Namespace, o.ID, prep.Layers)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			hold.Release()
		}
	}()

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
	rootfs := rootfsDir
	var remountedCaller []mount.Mount
	callerPath := ""
	switch {
	case callerMounted && prep.IDMap != "":
		// The container's root, an unprivileged host ID, may not reach
		// where the caller mounted it (Docker's directories are 0710), so
		// the idmapped overlay goes in the bundle, and is bound where the
		// caller expects it for its own use (docker cp).
		callerPath, rootfs = rootfsDir, filepath.Join(o.Bundle, "rootfs")
		if err := moveCaller(o.Rootfs, rootfs, prep.Layers, callerPath, callerMounts); err != nil {
			return nil, fmt.Errorf("mount rootfs: %w", err)
		}
		remountedCaller = callerMounts
		spec.Root.Path, rootfsDir = rootfs, rootfs
		defer func() {
			if retErr != nil {
				_ = unix.Unmount(callerPath, unix.MNT_DETACH)
				_ = mount.UnmountRecursive(rootfs, 0)
				_ = mountAll(callerMounts, callerPath)
			}
		}()
	case callerMounted && len(prep.Layers) > 0:
		if err := remountCaller(o.Rootfs, rootfs, prep.Layers, callerMounts); err != nil {
			return nil, fmt.Errorf("mount rootfs: %w", err)
		}
		remountedCaller = callerMounts
		defer func() {
			if retErr != nil {
				_ = remountCaller(callerMounts, rootfs, nil, nil)
			}
		}()
	case len(o.Rootfs) > 0 && !callerMounted:
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
		rootfsDir:      rootfsDir,
		callerMounted:  callerMounted,
		callerMounts:   remountedCaller,
		callerPath:     callerPath,
		snapshots:      hold,
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
		forkImages:     forkImages(prep),
		memoryMerge:    prep.MemoryMerge,
		tracking:       prep.TrackMemory,
		forkSignal:     prep.ForkSignal,
		criu:           prep.CRIU,
		gpu:            prep.GPU,
		gpuError:       prep.GPUError,
		status:         Created,
		exited:         make(chan struct{}),
		onExit:         onExit,
		onOOM:          o.OnOOM,
	}
	if o.Checkpoint != "" || prep.CheckpointFrom != nil {
		if !callerMounted {
			// Restore happens in Start, the way containerd expects.
			return c, nil
		}
		// CRI-O records the init's pid when it creates a container, and
		// reads /proc for it at once: it gets the restored one, frozen
		// until Start.
		if prep.CheckpointFrom != nil {
			return nil, fmt.Errorf("live migration into a container CRI-O runs: CRI-O needs the container's "+
				"process when it creates it, before the source has sent one: %w", errdefs.ErrNotImplemented)
		}
		if err := c.restore(c.checkpoint, c.callerNetwork()); err != nil {
			return nil, err
		}
		if err := c.lc.Pause(); err != nil {
			_ = c.lc.Signal(unix.SIGKILL)
			<-c.exited
			return nil, fmt.Errorf("freeze restored container: %w", err)
		}
		c.frozen = true
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
	// While the init waits to exec: a program killed by the OOM killer
	// in its first milliseconds is seen too.
	c.watchOOM()
	if err := c.track(proc); err != nil {
		return nil, err
	}
	return c, nil
}

// Incoming is what an incoming migration restores from: CRIU images, and
// for a post-copy one the page server (host:port) that serves its memory.
// Lazy restores without the memory, loaded from the images after.
type Incoming struct {
	Images     string
	PageServer string
	Lazy       bool
}

// Start runs the created container, or restores it from its checkpoint.
func (c *Container) Start(ctx context.Context) error {
	if c.checkpointFrom != nil {
		in, err := c.checkpointFrom(ctx)
		if err != nil {
			return err
		}
		c.checkpoint, c.remotePages = in.Images, in.PageServer
		c.lazyPages = c.lazyPages || in.Lazy
	}
	switch {
	case c.frozen:
		// Restored when created (callerMounted).
		if err := c.lc.Resume(); err != nil {
			return fmt.Errorf("thaw restored container: %w", err)
		}
		c.frozen = false
	case c.checkpoint == "":
		if err := c.lc.Exec(); err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		c.setStatus(Running)
		return nil
	default:
		if err := c.restore(c.checkpoint, c.callerNetwork()); err != nil {
			return err
		}
	}
	c.setStatus(Running)
	c.watchOOM()
	if c.forkSignal != 0 {
		if err := c.lc.Signal(c.forkSignal); err != nil {
			return fmt.Errorf("fork signal: %w", err)
		}
	}
	return nil
}

// restore starts a new incarnation from CRIU images.
// callerNetwork says whether the container's caller sets up its network,
// in the namespace the container makes, once the task exists: Docker
// does. Its devices (a veth to a host bridge) cannot be restored, and a
// new container must not have another's address anyway.
func (c *Container) callerNetwork() bool {
	if !c.callerMounted || ocispec.CRIO(c.spec) {
		return false
	}
	for _, ns := range c.cfg.Namespaces {
		if ns.Type == configs.NEWNET {
			return ns.Path == ""
		}
	}
	return false
}

// restore restores the container from images. With freshNetwork, its
// network namespace is restored empty but for its loopback, for the caller
// to set up (callerNetwork).
func (c *Container) restore(images string, freshNetwork bool) error {
	c.trackedSince = ""
	// The images may come from elsewhere (a snapshot, a migration): their
	// user namespace must be this container's.
	if err := userns.CheckImages(images, idExtents(c.cfg.UIDMappings), idExtents(c.cfg.GIDMappings)); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
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
	keepWork := false // the log of a lazy-pages daemon that runs on
	lazy := c.lazyPages || c.remotePages != ""
	began := time.Now()
	if lazy {
		// Must be serving before the restore asks it for a page, and it
		// keeps going afterwards: the container faults pages in as it
		// runs. It exits once it has sent the last one, or, serving only
		// faults (a fork), when the container is deleted. A post-copy
		// migration's pages come from the source, all of them, so the
		// source can go.
		onDemand := c.remotePages == "" && images == c.forkImages && lazyPagesOnDemand()
		d, err := startLazyPages(images, work, onDemand, c.remotePages)
		if err != nil {
			return err
		}
		c.lazyDaemon.stop()
		c.lazyDaemon, keepWork = d, true
	}
	daemonUp := time.Now()
	opts := &libcontainer.CriuOpts{
		ImagesDirectory: images,
		WorkDirectory:   work,
		LazyPages:       lazy,
		StatusFd:        -1,
		Command:         c.criuCommand(),
	}
	if freshNetwork {
		opts.EmptyNs = unix.CLONE_NEWNET
	}
	err = c.lc.Restore(proc, opts)
	if err != nil {
		c.io.detach() // restore requires pipes, never a terminal
		return fmt.Errorf("restore (criu log in %s): %w%s", work, err, criuErrors(filepath.Join(work, "restore.log")))
	}
	restored := time.Now()
	inCRIU, gaps := criuTimes(filepath.Join(work, "restore.log"), 3)
	defer func() {
		log.G(context.Background()).WithFields(log.Fields{
			"lazy":         lazy,
			"daemon":       daemonUp.Sub(began).Round(time.Millisecond),
			"criu":         restored.Sub(daemonUp).Round(time.Millisecond),
			"criu-own":     inCRIU.Round(time.Millisecond),
			"criu-longest": gaps,
			"after":        time.Since(restored).Round(time.Millisecond),
		}).Info("restore took")
	}()
	if freshNetwork {
		pid, err := proc.Pid()
		if err == nil {
			err = loopbackUp(pid)
		}
		if err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	}
	if !keepWork {
		_ = os.RemoveAll(work)
	}
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
	if err := c.fillMemory(); err != nil {
		return err
	}
	c.trackedSince = ""
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
		return fmt.Errorf("checkpoint (criu log in %s): %w%s", work, err, criuErrors(filepath.Join(work, "dump.log")))
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
func (c *Container) Snapshot(ctx context.Context, dir string) error {
	return c.snapshot(ctx, dir, "")
}

// Point is Snapshot for a point on the timeline: with parent, the
// directory of an earlier point, only the memory written since that one
// is dumped, the rest read from it on a restore. Without memory tracking
// (MemoryTracking), points are full.
func (c *Container) Point(ctx context.Context, dir, parent string) error {
	return c.snapshot(ctx, dir, parent)
}

// MemoryTracking says whether the kernel tracks which pages a process
// wrote (soft-dirty bits), which incremental dumps need; arm64 does not.
var MemoryTracking = sync.OnceValue(func() bool {
	return exec.Command("criu", "check", "--feature", "mem_dirty_track").Run() == nil
})

func (c *Container) snapshot(ctx context.Context, dir, parent string) (retErr error) {
	c.op.Lock()
	defer c.op.Unlock()
	if err := c.fillMemory(); err != nil {
		return err
	}
	// Pages written since parent are known only if it was the last dump:
	// any other (a snapshot, a checkpoint, a migration's) or a restore
	// started tracking afresh.
	if !c.tracking || parent != c.trackedSince {
		parent = ""
	}
	c.trackedSince = ""
	defer func() {
		if retErr == nil && c.tracking {
			c.trackedSince = dir
		}
	}()
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
	// The snapshot holds the container's memory, secrets and all: root's
	// alone.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if err := copyTree(upper, filepath.Join(dir, "upper")); err != nil {
		return fmt.Errorf("copy writable layer: %w", err)
	}
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	opts := &libcontainer.CriuOpts{
		ImagesDirectory: filepath.Join(dir, "images"),
		WorkDirectory:   work,
		LeaveRunning:    true,
	}
	if parent != "" {
		// Relative to the images directory; CRIU links it as "parent"
		// and follows it on a restore.
		rel, err := filepath.Rel(opts.ImagesDirectory, filepath.Join(parent, "images"))
		if err != nil {
			return err
		}
		opts.ParentImage = rel // and track memory, for the next one
	}
	err = c.lc.Checkpoint(opts)
	if err != nil {
		return fmt.Errorf("checkpoint (criu log in %s): %w%s", work, err, criuErrors(filepath.Join(work, "dump.log")))
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
	if c.callerNetwork() && hasNetDevices(c.Pid()) {
		// A rewind restores the processes in a network namespace of
		// their own, without the devices the caller set up in this one.
		return fmt.Errorf("rewind of a container on a network its caller set up (Docker's bridge networks; "+
			"run it with --network none or host): %w", errdefs.ErrNotImplemented)
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
		// As at any exit, the output ends: Docker holds the container
		// until it reads EOF on it.
		if c.tty != nil {
			c.tty.Close()
		} else {
			c.io.Close()
		}
		c.markExited(nil)
		return err
	}
	c.mu.Lock()
	c.replacing = false
	c.mu.Unlock()
	return nil
}

func (c *Container) replace(upper, dir string) error {
	// Its pages are the old processes'.
	c.lazyDaemon.stop()
	c.lazyDaemon = nil
	if err := c.lc.Destroy(); err != nil {
		return fmt.Errorf("destroy old container: %w", err)
	}
	rootfs := c.rootfsDir
	if err := mount.UnmountRecursive(rootfs, 0); err != nil {
		return fmt.Errorf("unmount rootfs: %w", err)
	}
	if c.callerMounted {
		if err := clearVolatile(c.rootfs); err != nil {
			return err
		}
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
	if c.callerPath != "" {
		// The bind still shows the old overlay.
		_ = unix.Unmount(c.callerPath, unix.MNT_DETACH)
		if err := unix.Mount(rootfs, c.callerPath, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind rootfs at %s: %w", c.callerPath, err)
		}
	}
	lc, err := libcontainer.Create(Root(c.namespace), c.ID, c.cfg)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	c.lc = lc
	return c.restore(filepath.Join(dir, "images"), false)
}

// moveCaller mounts the overlay its caller mounted at callerPath at dir
// instead, as mounts (idmapped) with layers, and binds it at callerPath.
// On failure the caller's own mount, orig, is put back.
func moveCaller(mounts []mount.Mount, dir string, layers []string, callerPath string, orig []mount.Mount) (retErr error) {
	if err := unix.Unmount(callerPath, 0); err != nil {
		return fmt.Errorf("unmount %s: %w", callerPath, err)
	}
	defer func() {
		if retErr != nil {
			_ = mountAll(orig, callerPath)
		}
	}()
	if err := clearVolatile(mounts); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return err
	}
	if err := mountRootfs(mounts, dir, layers); err != nil {
		return err
	}
	if err := unix.Mount(dir, callerPath, "", unix.MS_BIND, ""); err != nil {
		_ = mount.UnmountRecursive(dir, 0)
		return fmt.Errorf("bind rootfs at %s: %w", callerPath, err)
	}
	return nil
}

// remountCaller mounts the overlay its caller mounted at dir again, with
// layers between its writable layer and the image's (none: as the caller
// had it).
// On failure it mounts orig, the caller's own mounts, back if given.
func remountCaller(mounts []mount.Mount, dir string, layers []string, orig []mount.Mount) error {
	if err := unix.Unmount(dir, 0); err != nil {
		return fmt.Errorf("unmount %s: %w", dir, err)
	}
	if err := clearVolatile(mounts); err != nil {
		return err
	}
	if err := mountRootfs(mounts, dir, layers); err != nil {
		if orig != nil {
			_ = mountAll(orig, dir)
		}
		return err
	}
	return nil
}

// clearVolatile lets a volatile overlay (CRI-O mounts them so) be mounted
// again. Its work directory keeps a mark that the kernel refuses to mount
// over: after a crash the writable layer may be inconsistent, and whoever
// mounts it must say it is not. An overlay just unmounted cleanly is not,
// so the mark goes, as containers/storage removes it too.
func clearVolatile(mounts []mount.Mount) error {
	var work string
	volatile := false
	for _, o := range mounts[0].Options {
		if v, ok := strings.CutPrefix(o, "workdir="); ok {
			work = v
		} else if o == "volatile" || o == "fsync=volatile" {
			volatile = true
		}
	}
	if !volatile || work == "" {
		return nil
	}
	// The kernel's own directory, under the work directory only the
	// kernel writes.
	if err := os.RemoveAll(filepath.Join(work, "work", "incompat", "volatile")); err != nil {
		return fmt.Errorf("clear volatile overlay mark: %w", err)
	}
	return nil
}

// unmountRootfs undoes what gcr mounted for the rootfs. Where the caller
// mounted it, the caller unmounts it: gcr puts back the caller's own
// mount if it mounted another there, and nothing if the caller has
// unmounted it already (CRI-O does when the container stops).
func (c *Container) unmountRootfs() error {
	if c.callerPath != "" {
		// The caller's mount goes back unless it has unmounted the bind.
		bound := callerRootfs(c.callerPath) != nil
		if bound {
			if err := unix.Unmount(c.callerPath, unix.MNT_DETACH); err != nil {
				return err
			}
		}
		if err := mount.UnmountRecursive(c.rootfsDir, 0); err != nil {
			return err
		}
		if !bound {
			return nil
		}
		if err := clearVolatile(c.callerMounts); err != nil {
			return err
		}
		return mountAll(c.callerMounts, c.callerPath)
	}
	if !c.callerMounted {
		return mount.UnmountRecursive(c.rootfsDir, 0)
	}
	if c.callerMounts == nil || callerRootfs(c.rootfsDir) == nil {
		return nil
	}
	return remountCaller(c.callerMounts, c.rootfsDir, nil, nil)
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
	if err := c.fillMemory(); err != nil {
		return err
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
	if err := c.fillMemory(); err != nil {
		return err
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
	c.trackedSince = ""
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

// LazyDump freezes the container and dumps it for a post-copy migration:
// everything but its memory to dir, its memory served by CRIU at host:port
// to a lazy-pages daemon. It returns once CRIU serves. wait returns once
// every page was fetched, after which CRIU has killed the container's
// processes; abort, before then, stops CRIU and thaws the container,
// which runs on.
func (c *Container) LazyDump(ctx context.Context, dir, host string, port int) (wait, abort func() error, err error) {
	c.op.Lock()
	unlock := sync.OnceFunc(c.op.Unlock)
	fail := func(err error) (func() error, func() error, error) {
		unlock()
		return nil, nil, err
	}
	if c.gpu != nil || c.gpuError != nil {
		// CRIU's CUDA plugin cannot serve GPU memory lazily.
		return fail(fmt.Errorf("post-copy migration of a container with GPUs: %w", errdefs.ErrNotImplemented))
	}
	if err := c.fillMemory(); err != nil {
		return fail(err)
	}
	c.trackedSince = ""
	ready, signal, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	defer ready.Close()
	// libcontainer closes the status fd once CRIU is ready, by number.
	statusFd, err := unix.Dup(int(signal.Fd()))
	signal.Close()
	if err != nil {
		return fail(err)
	}
	if err := c.lc.Pause(); err != nil {
		unix.Close(statusFd)
		return fail(fmt.Errorf("freeze: %w", err))
	}
	init := c.Pid()
	done := make(chan error, 1)
	go func() {
		err := c.lc.Checkpoint(&libcontainer.CriuOpts{
			ImagesDirectory: dir,
			WorkDirectory:   dir,
			LazyPages:       true,
			StatusFd:        statusFd,
			PageServer:      libcontainer.CriuPageServerInfo{Address: host, Port: int32(port)},
		})
		if err != nil {
			err = fmt.Errorf("criu dump: %w%s", err, criuErrors(filepath.Join(dir, "dump.log")))
		}
		done <- err
	}()
	served := make(chan bool, 1)
	go func() {
		var b [1]byte
		n, _ := ready.Read(b[:])
		served <- n == 1
	}()
	select {
	case ok := <-served:
		if !ok {
			err := <-done
			_ = c.lc.Resume()
			return fail(fmt.Errorf("lazy dump: CRIU did not serve: %v", err))
		}
	case err := <-done:
		// Failed before serving: the status fd is still ours.
		unix.Close(statusFd)
		_ = c.lc.Resume()
		return fail(err)
	}
	var once sync.Once
	var result error
	finish := func() error {
		once.Do(func() { result = <-done })
		return result
	}
	wait = func() error {
		defer unlock()
		return finish()
	}
	abort = func() error {
		defer unlock()
		// CRIU serves with the container's tasks seized: their tracer.
		if tracer := tracerOf(init); tracer > 0 {
			_ = unix.Kill(tracer, unix.SIGKILL)
		}
		_ = finish()
		// Thawed already if the dump's failure did.
		if err := c.lc.Resume(); err != nil && !errors.Is(err, libcontainer.ErrNotPaused) {
			return err
		}
		return nil
	}
	return wait, abort, nil
}

// tracerOf is the pid tracing pid, 0 if none.
func tracerOf(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "TracerPid:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
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

// forkImages are the images a fork restores, "" for other containers.
func forkImages(prep Prepared) string {
	if prep.ForkFrom == "" {
		return ""
	}
	return filepath.Join(prep.ForkFrom, "images")
}

// lazyPagesOnDemand says whether criu's lazy-pages daemon can serve only
// faults (makhov/criu: --lazy-pages-on-demand).
var lazyPagesOnDemand = sync.OnceValue(func() bool {
	out, _ := exec.Command("criu", "--help").CombinedOutput()
	return strings.Contains(string(out), "--lazy-pages-on-demand")
})

// lazyDaemon is a criu lazy-pages daemon.
type lazyDaemon struct {
	cmd      *exec.Cmd
	onDemand bool
	done     chan struct{}
	err      error // how it exited, once done
	stopping atomic.Bool
	log      string
}

// drain has the daemon copy the pages it has not, and waits until it has
// and exited. A daemon serving faults only is asked to (SIGUSR1); one
// that copies in the background is let finish.
func (d *lazyDaemon) drain(timeout time.Duration) error {
	if d.onDemand {
		if err := d.cmd.Process.Signal(unix.SIGUSR1); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	select {
	case <-d.done:
		return d.err
	case <-time.After(timeout):
		return fmt.Errorf("lazy pages: not done after %s", timeout)
	}
}

// fillMemory has the container's lazy-pages daemon copy in the pages the
// container has not touched, and waits until it has. A dump must come
// after: CRIU takes a page nobody copied in for one never written, and
// leaves it out.
func (c *Container) fillMemory() error {
	if c.lazyDaemon == nil {
		return nil
	}
	if err := c.lazyDaemon.drain(10 * time.Minute); err != nil {
		return fmt.Errorf("fill the container's memory before dumping it: %w", err)
	}
	c.lazyDaemon = nil
	return nil
}

// stop stops the daemon, if it still runs; nil does nothing.
func (d *lazyDaemon) stop() {
	if d == nil {
		return
	}
	d.stopping.Store(true)
	select {
	case <-d.done:
		return
	default:
	}
	if !d.onDemand {
		// It exits once it has copied every page: still running, it has
		// not, and the container may be stuck on a fault it never served.
		log.G(context.Background()).WithField("log", d.log).
			Warn("lazy pages daemon stopped before it copied every page: " + logTail(d.log, 15))
	}
	_ = d.cmd.Process.Signal(unix.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.done
	}
}

// startLazyPages runs a criu lazy-pages daemon that serves the images'
// pages to a restore. Both sides find each other through a socket named
// relative to the work directory, so they must share one. The daemon
// outlives the restore: it copies the pages the restored processes do not
// touch in the background, and exits once it has sent the last; or, with
// onDemand, it copies none, and runs until stopped or drained.
func startLazyPages(images, work string, onDemand bool, pageServer string) (*lazyDaemon, error) {
	ready, signal, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer signal.Close()
	// Its log, in the work directory: every page it copies or zero-fills
	// and every fault, where that is bounded by the memory (it copies all
	// of it, then exits); warnings and errors only for one that serves
	// faults for as long as the container runs, unless the shim runs with
	// GCR_LAZY_PAGES_DEBUG set.
	args := []string{"lazy-pages", "--images-dir", images, "--work-dir", work, "--status-fd", "3",
		"-o", "lazy-pages.log", "-v4"}
	if onDemand {
		args = append(args, "--lazy-pages-on-demand")
		if os.Getenv("GCR_LAZY_PAGES_DEBUG") == "" {
			args = append(args, "-v2")
		}
	}
	if pageServer != "" {
		host, port, err := net.SplitHostPort(pageServer)
		if err != nil {
			return nil, err
		}
		args = append(args, "--page-server", "--address", host, "--port", port)
	}
	cmd := exec.Command("criu", args...)
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
	started := time.Now()
	d := &lazyDaemon{cmd: cmd, onDemand: onDemand, done: make(chan struct{}), log: filepath.Join(work, "lazy-pages.log")}
	go func() {
		defer close(d.done)
		d.err = cmd.Wait()
		if d.err == nil && !d.stopping.Load() {
			log.G(context.Background()).WithField("took", time.Since(started).Round(time.Millisecond)).
				Info("lazy pages daemon copied every page")
		}
		if err := d.err; err != nil && !d.stopping.Load() {
			// The container is left with pages nobody will send it; it
			// gets SIGBUS when it touches one.
			log.G(context.Background()).WithError(err).
				WithField("log", filepath.Join(work, "lazy-pages.log")).Error("lazy pages daemon stopped early")
		}
	}()
	return d, nil
}

// criuTimes reads a CRIU log's timestamps ("(00.123456)" opening each
// line): how long CRIU took by its own clock, and its n longest gaps
// between lines, each with the line that ended it.
func criuTimes(path string, n int) (time.Duration, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, ""
	}
	type gap struct {
		d    time.Duration
		line string
	}
	var gaps []gap
	var last time.Duration
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		end := strings.IndexByte(l, ')')
		if !strings.HasPrefix(l, "(") || end < 0 {
			continue
		}
		secs, err := strconv.ParseFloat(l[1:end], 64)
		if err != nil {
			continue
		}
		at := time.Duration(secs * float64(time.Second))
		gaps = append(gaps, gap{at - last, strings.TrimSpace(l[end+1:])})
		last = at
	}
	slices.SortFunc(gaps, func(a, b gap) int { return int(b.d - a.d) })
	var out []string
	for _, g := range gaps[:min(n, len(gaps))] {
		line := g.line
		if len(line) > 100 {
			line = line[:100]
		}
		out = append(out, fmt.Sprintf("%s before %q", g.d.Round(time.Millisecond), line))
	}
	return last, strings.Join(out, "; ")
}

// logTail returns the last n lines of a log, joined with " | ".
func logTail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
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
// idmaps the lower layers, and the upper layer is given to the container's
// root so files it creates are owned by the mapped IDs.
func idmapRootfs(mounts []mount.Mount, idmap string, root int) ([]mount.Mount, error) {
	if len(mounts) == 0 {
		return nil, errRootfs("gcr's user namespaces")
	}
	size := 0
	if f := strings.Split(idmap, ":"); len(f) == 3 {
		size, _ = strconv.Atoi(f[2])
	}
	if size <= 0 {
		return nil, fmt.Errorf("idmap %q: %w", idmap, errdefs.ErrInvalidArgument)
	}
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
			if err := shiftUpper(upper, root, size); err != nil {
				return nil, fmt.Errorf("give writable layer to the container's root: %w", err)
			}
		}
		out[i].Options = append(slices.Clone(m.Options), "uidmap="+idmap, "gidmap="+idmap)
	}
	return out, nil
}

// hasNetDevices says whether pid's network namespace has devices other
// than its loopback.
func hasNetDevices(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/dev", pid))
	if err != nil {
		return false
	}
	lines := strings.Split(string(b), "\n")
	for _, line := range lines[min(2, len(lines)):] { // two header lines
		if name, _, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && name != "lo" {
			return true
		}
	}
	return false
}

// loopbackUp brings lo up in pid's network namespace.
func loopbackUp(pid int) error {
	errc := make(chan error, 1)
	go func() {
		// This thread switches network namespace and is never reused.
		runtime.LockOSThread()
		errc <- func() error {
			ns, err := unix.Open(fmt.Sprintf("/proc/%d/ns/net", pid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			defer unix.Close(ns)
			if err := unix.Setns(ns, unix.CLONE_NEWNET); err != nil {
				return fmt.Errorf("enter network namespace: %w", err)
			}
			fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				return err
			}
			defer unix.Close(fd)
			ifr, err := unix.NewIfreq("lo")
			if err != nil {
				return err
			}
			if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
				return fmt.Errorf("lo: %w", err)
			}
			ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
			if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
				return fmt.Errorf("lo up: %w", err)
			}
			return nil
		}()
	}()
	return <-errc
}

// shiftUpper moves what in a writable layer is owned by IDs below size
// (unmapped: the host's) into the range at root, as the idmapped image
// layers beneath it appear. The layer is empty but for what the caller put
// there before the container started (Docker: mountpoints such as
// /etc/hosts); what the container wrote is in the range already, so a
// container started again is left as it is.
func shiftUpper(upper string, root, size int) error {
	return filepath.WalkDir(upper, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		uid, gid := int(st.Uid), int(st.Gid)
		if uid >= size && gid >= size {
			return nil
		}
		if uid < size {
			uid += root
		}
		if gid < size {
			gid += root
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return err
		}
		// chown clears setuid and setgid.
		if mode := info.Mode(); mode&(fs.ModeSetuid|fs.ModeSetgid) != 0 && mode&fs.ModeSymlink == 0 {
			return os.Chmod(path, mode)
		}
		return nil
	})
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
	return "", errRootfs("snapshots, time travel and migration")
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
	c.lazyDaemon.stop()
	c.lazyDaemon = nil
	if err := c.unmountRootfs(); err != nil {
		errs = append(errs, err)
	} else {
		// Kept while a layer may still be mounted.
		c.snapshots.Release()
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
	c.noteOOMAtExit(exitStatus(st) == 128+uint32(syscall.SIGKILL))
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

// errRootfs says that what needs gcr to mount the container's overlay
// rootfs cannot have it: the caller mounted it (CRI-O does; snapshots and
// forks work with the overlay it mounted, user namespaces are not tried),
// or it is not an overlay.
func errRootfs(what string) error {
	return fmt.Errorf("%s need the rootfs as overlay mounts from containerd, which gcr mounts itself; "+
		"this container's was mounted by its caller (CRI-O): %w", what, errdefs.ErrNotImplemented)
}

func idExtents(m []configs.IDMap) []userns.Extent {
	out := make([]userns.Extent, len(m))
	for i, e := range m {
		out[i] = userns.Extent{ContainerID: uint32(e.ContainerID), HostID: uint32(e.HostID), Size: uint32(e.Size)}
	}
	return out
}
