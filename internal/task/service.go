//go:build linux

// Package task implements containerd's task API on top of internal/container.
package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/runc/options"
	apitask "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runc/libcontainer/seccomp"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/config"
	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/gpu"
	"github.com/makhov/gcr/internal/hardening"
	"github.com/makhov/gcr/internal/landlock"
	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/netpolicy"
	"github.com/makhov/gcr/internal/supervisor"
	"github.com/makhov/gcr/internal/timetravel"
	"github.com/makhov/gcr/internal/userns"
)

// stateRoot holds per-container state (timelines, migration rounds) by
// namespace and container id.
const stateRoot = "/var/lib/gcr"

var (
	_     = shim.TTRPCService(&service{})
	empty = &ptypes.Empty{}
)

type service struct {
	ctx       context.Context
	publisher shim.Publisher
	shutdown  shutdown.Service

	mu         sync.Mutex
	containers map[string]*container.Container
	extras     map[string]*extras

	// The node's netlink policy, loaded for the first hardened container.
	netPolicyOnce sync.Once
	netPolicy     *netpolicy.Policy
}

// netlinkPolicy loads the netlink policy once; nil when the kernel cannot
// apply it, logged once.
func (s *service) netlinkPolicy(ctx context.Context) *netpolicy.Policy {
	s.netPolicyOnce.Do(func() {
		p, err := netpolicy.Load()
		if err != nil {
			log.G(ctx).WithError(err).Warn("netlink policy off: containers' network configuration is limited by seccomp alone")
			return
		}
		s.netPolicy = p
	})
	return s.netPolicy
}

// extras is what a container has beyond containerd's task API.
type extras struct {
	recorder   *timetravel.Recorder   // time travel, if enabled
	supervisor *supervisor.Supervisor // if the filter hands calls to user space
	receiver   *migrate.Receiver      // incoming migration, if requested
	userns     *userns.Range          // the container's user namespace range
	control    *control.Server
	stateDir   string
	id         string
	// netPolicy covers cgroupID, if not 0.
	netPolicy *netpolicy.Policy
	cgroupID  uint64
}

func (e *extras) Close() {
	if e.netPolicy != nil && e.cgroupID != 0 {
		_ = e.netPolicy.Remove(e.cgroupID)
	}
	if e.control != nil {
		e.control.Close()
	}
	if e.receiver != nil {
		e.receiver.Close()
	}
	if e.supervisor != nil {
		e.supervisor.Close()
	}
	if e.recorder != nil {
		_ = e.recorder.Close()
	}
	if e.userns != nil {
		_ = userns.Release(*e.userns, e.id)
	}
	_ = os.RemoveAll(e.stateDir)
}

func NewTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
	s := &service{
		ctx:        context.WithoutCancel(ctx),
		publisher:  publisher,
		shutdown:   sd,
		containers: make(map[string]*container.Container),
		extras:     make(map[string]*extras),
	}
	sd.RegisterCallback(func(context.Context) error {
		publisher.Close()
		return nil
	})
	return s, nil
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTTRPCTaskService(server, s)
	return nil
}

func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	if r.Terminal {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "terminal")
	}
	var rootfs []mount.Mount
	for _, m := range r.Rootfs {
		rootfs = append(rootfs, mount.Mount{Type: m.Type, Source: m.Source, Target: m.Target, Options: m.Options})
	}

	cfg, err := config.FromOptions(r.Options)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	ex := &extras{stateDir: filepath.Join(stateRoot, ns, r.ID), id: r.ID}
	var c *container.Container
	opts := container.CreateOpts{
		ID:         r.ID,
		Namespace:  ns,
		Bundle:     r.Bundle,
		Rootfs:     rootfs,
		Stdin:      r.Stdin,
		Stdout:     r.Stdout,
		Stderr:     r.Stderr,
		Checkpoint: r.Checkpoint,
	}
	var hardened bool
	opts.PrepareSpec = func(spec *specs.Spec) (container.Prepared, error) {
		prep := container.Prepared{IOOwner: cfg.IOUID}
		forkFrom, forkKey, err := forkSource(spec)
		if err != nil {
			return prep, err
		}
		prep.ForkFrom = forkFrom
		if v, ok := spec.Annotations[container.AnnotationForkSignal]; ok {
			if forkFrom == "" {
				return prep, fmt.Errorf("%s needs %s: %w", container.AnnotationForkSignal,
					container.AnnotationForkFrom, errdefs.ErrInvalidArgument)
			}
			if prep.ForkSignal, err = container.ParseSignal(v); err != nil {
				return prep, err
			}
		}
		switch {
		case forkFrom != "":
			prep.Layers, err = container.ForkLayers(forkFrom)
		case r.Checkpoint != "":
			prep.Layers, err = container.ReadLayers(r.Checkpoint)
		}
		if err != nil {
			return prep, fmt.Errorf("layers: %w", err)
		}
		if v, ok := spec.Annotations[container.AnnotationLazyRestore]; ok {
			if prep.LazyPages, err = strconv.ParseBool(v); err != nil {
				return prep, fmt.Errorf("%s=%q: %w", container.AnnotationLazyRestore, v, errdefs.ErrInvalidArgument)
			}
			if prep.LazyPages && forkFrom == "" && r.Checkpoint == "" {
				return prep, fmt.Errorf("%s needs a container that is restored or forked: %w",
					container.AnnotationLazyRestore, errdefs.ErrInvalidArgument)
			}
			if prep.LazyPages && !container.LazyPagesAvailable() {
				// Said here rather than left to a CRIU log: the node has to
				// allow it, and an operator should know what they are
				// allowing (see the README).
				return prep, fmt.Errorf("%s: this node has vm.unprivileged_userfaultfd = 0, so a container "+
					"in a user namespace cannot open the userfaultfd a lazy restore needs; set it to 1 to "+
					"allow lazy restores: %w", container.AnnotationLazyRestore, errdefs.ErrFailedPrecondition)
			}
		}
		if v, ok := spec.Annotations[container.AnnotationMemoryMerge]; ok {
			if prep.MemoryMerge, err = strconv.ParseBool(v); err != nil {
				return prep, fmt.Errorf("%s=%q: %w", container.AnnotationMemoryMerge, v, errdefs.ErrInvalidArgument)
			}
			if prep.MemoryMerge {
				if err := container.MemoryMergeAvailable(); err != nil {
					return prep, fmt.Errorf("%s: %v: %w", container.AnnotationMemoryMerge, err, errdefs.ErrFailedPrecondition)
				}
			}
			// The container's init reads it back from its labels, which
			// are the annotations as written.
			spec.Annotations[container.AnnotationMemoryMerge] = strconv.FormatBool(prep.MemoryMerge)
		}
		ok, why, err := userns.Decide(spec, cfg.UserNamespaces)
		if err != nil {
			return prep, err
		}
		if ok && forkKey != "" {
			// The snapshot's writable layer, which the fork keeps
			// underneath its own, holds host IDs from the range its
			// container mapped; the fork maps the same one.
			rng, err := userns.Join(forkKey, r.ID)
			if err != nil {
				return prep, fmt.Errorf("fork: %w", err)
			}
			ex.userns = &rng
			userns.Apply(spec, rng)
			if err := userns.OpenNetwork(spec); err != nil {
				return prep, fmt.Errorf("fork: pod network: %w", err)
			}
			prep.IDMap, prep.IDMapRoot, prep.IDKey = rng.IDMapOption(), int(rng.Base), rng.Key
		} else if ok {
			var (
				want uint32
				err  error
			)
			if v, ok := spec.Annotations[userns.AnnotationBase]; ok {
				if want, err = userns.ParseBase(v); err != nil {
					return prep, err
				}
			}
			if r.Checkpoint != "" {
				if b, err := os.ReadFile(filepath.Join(r.Checkpoint, userns.BaseFile)); err == nil {
					if want, err = userns.ParseBase(string(b)); err != nil {
						return prep, err
					}
				}
			}
			rng, err := userns.Allocate(spec, ns, r.ID, want)
			if err != nil {
				return prep, fmt.Errorf("user namespace: %w", err)
			}
			ex.userns = &rng
			userns.Apply(spec, rng)
			if err := userns.OpenNetwork(spec); err != nil {
				return prep, fmt.Errorf("user namespace: pod network: %w", err)
			}
			prep.IDMap, prep.IDMapRoot, prep.IDKey = rng.IDMapOption(), int(rng.Base), rng.Key
		} else {
			log.G(ctx).WithField("reason", why).Debug("no user namespace")
		}
		// gcr's user namespace or one Kubernetes asked for.
		prep.CgroupOwner = userns.DelegateCgroup(spec)
		if addr, ok := spec.Annotations[migrate.AnnotationIn]; ok {
			recv, err := migrate.NewReceiver(s.ctx, addr, spec.Annotations[migrate.AnnotationToken],
				filepath.Join(ex.stateDir, "migrate-in"),
				func() (string, error) { return c.UpperDir() },
				func() uint32 { return c.IDBase() },
				func() []string { return c.Layers() })
			if err != nil {
				return prep, fmt.Errorf("migration receiver: %w", err)
			}
			ex.receiver = recv
			prep.CheckpointFrom = recv.Wait
		}
		// Calls the container's seccomp filter hands to its supervisor.
		handlers := map[string]supervisor.Handler{}
		// Landlock first: hardening must know it (a confined container
		// may not mount).
		whyNotLandlocked := landlock.Decide(spec, cfg.Landlock)
		landlocked := whyNotLandlocked == ""
		hard := hardening.Apply(spec, hardening.Options{
			Level: cfg.Hardening, NestedUserNamespaces: cfg.NestedUserNamespaces, Landlocked: landlocked,
		})
		hardened = hard.Skipped == ""
		if !hardened {
			log.G(ctx).WithField("reason", hard.Skipped).Debug("not hardened")
		}
		for _, name := range hard.Supervised {
			handlers[name] = hardening.Handlers[name]
		}
		// Options for this container's restores. libcontainer restores
		// through CRIU's RPC, which has no fields for them; a config file
		// that org.criu.config names does.
		var criuOpts []string
		if !landlocked {
			log.G(ctx).WithField("reason", whyNotLandlocked).Debug("no Landlock")
		} else if opt, ok := landlock.CRIUOption(); ok {
			criuOpts = append(criuOpts, opt)
		} else {
			log.G(ctx).Warn("CRIU cannot restore Landlock (no --landlock-fs): restored containers will not be confined")
		}
		var restoreOpts []string
		if minors := gpu.Minors(spec); len(minors) > 0 {
			prep.GPU, prep.GPUError = gpu.Probe("/", minors)
			if prep.GPUError != nil {
				log.G(ctx).WithError(prep.GPUError).Warn("GPUs not described: checkpoints refused")
			} else {
				restoreOpts = gpu.RestoreCRIUOptions
			}
		}
		policy, err := timetravel.ParsePolicy(spec.Annotations)
		if err != nil {
			return prep, err
		}
		if policy != nil {
			if len(gpu.Minors(spec)) > 0 {
				// Each point would copy GPU memory, and CRIU's CUDA plugin
				// cannot take incremental dumps.
				return prep, fmt.Errorf("time travel for containers with GPUs: %w", errdefs.ErrNotImplemented)
			}
			if spec.Process.Terminal {
				return prep, fmt.Errorf("time travel needs a container without a terminal: %w", errdefs.ErrNotImplemented)
			}
			rec, err := timetravel.NewRecorder(policy, filepath.Join(ex.stateDir, "time-travel"))
			if err != nil {
				return prep, fmt.Errorf("time travel: %w", err)
			}
			ex.recorder = rec
			policy.AddTrigger(spec)
			for _, name := range policy.Syscalls {
				handlers[name] = rec.Handle
			}
		}
		if len(handlers) > 0 {
			sup, err := supervisor.New(s.ctx, strings.TrimSuffix(control.SocketPath(ns, r.ID), ".sock")+".sup", handlers)
			if err != nil {
				return prep, fmt.Errorf("supervisor: %w", err)
			}
			ex.supervisor = sup
			spec.Linux.Seccomp.ListenerPath = sup.HandoffPath
			// Once the supervisor has taken a call, the caller waits for
			// the answer without being interrupted by signals (short of
			// being killed). Otherwise a signal (Go's preemption SIGURG,
			// in a nested runc) makes the kernel restart the call after
			// the supervisor already performed it: a mount done twice,
			// the second failing with EBUSY.
			if !waitKillable() {
				log.G(ctx).Debug("no SECCOMP_FILTER_FLAG_WAIT_KILLABLE_RECV (libseccomp < 2.6): a signal can make a supervised call run twice")
			} else if !slices.Contains(spec.Linux.Seccomp.Flags, specs.LinuxSeccompFlagWaitKillableRecv) {
				spec.Linux.Seccomp.Flags = append(spec.Linux.Seccomp.Flags, specs.LinuxSeccompFlagWaitKillableRecv)
			}
			// Restores hand the restored listeners back to this supervisor.
			criuOpts = append(criuOpts, sup.CRIUOption())
		}
		if len(criuOpts) > 0 || len(restoreOpts) > 0 {
			prep.CRIU = container.CRIUConfig{
				Path:           filepath.Join(ex.stateDir, "criu.conf"),
				Options:        criuOpts,
				RestoreOptions: restoreOpts,
			}
		}
		return prep, nil
	}
	c, err = container.Create(ctx, opts, s.handleExit)
	if err != nil {
		ex.Close()
		return nil, errgrpc.ToGRPC(err)
	}
	if hardened && cfg.NetlinkPolicy != netpolicy.Off {
		ex.netPolicy = s.netlinkPolicy(ctx)
		// A restore has no cgroup before Start; Start covers it then.
		if err := ex.coverCgroup(c); err != nil {
			log.G(ctx).WithError(err).Debug("netlink policy: covering the container at start instead")
		}
	}

	h := control.Handlers{
		Migrate: func(ctx context.Context, target, token string) (migrate.Result, error) {
			return s.migrateOut(ctx, c, ex, target, token)
		},
		Snapshot: c.Snapshot,
	}
	if ex.recorder != nil {
		ex.recorder.Attach(c)
		h.Timeline = ex.recorder
	}
	if ex.control, err = control.Serve(s.ctx, ns, r.ID, h); err != nil {
		log.G(ctx).WithError(err).Error("control socket")
	}

	s.mu.Lock()
	s.containers[r.ID] = c
	s.extras[r.ID] = ex
	s.mu.Unlock()

	s.send(&eventstypes.TaskCreate{
		ContainerID: r.ID,
		Bundle:      r.Bundle,
		Rootfs:      r.Rootfs,
		IO:          &eventstypes.TaskIO{Stdin: r.Stdin, Stdout: r.Stdout, Stderr: r.Stderr},
		Checkpoint:  r.Checkpoint,
		Pid:         uint32(c.Pid()),
	})
	return &taskAPI.CreateTaskResponse{Pid: uint32(c.Pid())}, nil
}

func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	if r.ExecID != "" {
		e, err := s.getExec(r.ID, r.ExecID)
		if err != nil {
			return nil, err
		}
		if err := e.Start(ctx); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
		s.send(&eventstypes.TaskExecStarted{ContainerID: r.ID, ExecID: e.ID, Pid: uint32(e.Pid())})
		return &taskAPI.StartResponse{Pid: uint32(e.Pid())}, nil
	}
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	err = c.Start(ctx)
	s.mu.Lock()
	ex := s.extras[r.ID]
	s.mu.Unlock()
	if ex != nil && ex.receiver != nil {
		// Tell the migration source how the restore went; it keeps its copy
		// running unless this succeeded.
		ex.receiver.Done(err)
	}
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	if ex != nil {
		if err := ex.coverCgroup(c); err != nil {
			log.G(ctx).WithError(err).Warn("netlink policy: container not covered")
		}
	}
	s.send(&eventstypes.TaskStart{ContainerID: c.ID, Pid: uint32(c.Pid())})
	return &taskAPI.StartResponse{Pid: uint32(c.Pid())}, nil
}

func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	if r.ExecID != "" {
		e, err := s.getExec(r.ID, r.ExecID)
		if err != nil {
			return nil, err
		}
		if err := e.Delete(); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
		status, at := e.Exit()
		return &taskAPI.DeleteResponse{Pid: uint32(e.Pid()), ExitStatus: status, ExitedAt: protobuf.ToTimestamp(at)}, nil
	}
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	if err := c.Delete(ctx); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	s.mu.Lock()
	delete(s.containers, r.ID)
	ex := s.extras[r.ID]
	delete(s.extras, r.ID)
	s.mu.Unlock()
	if ex != nil {
		ex.Close()
	}

	status, at := c.Exit()
	s.send(&eventstypes.TaskDelete{
		ContainerID: c.ID,
		Pid:         uint32(c.Pid()),
		ExitStatus:  status,
		ExitedAt:    protobuf.ToTimestamp(at),
	})
	return &taskAPI.DeleteResponse{
		Pid:        uint32(c.Pid()),
		ExitStatus: status,
		ExitedAt:   protobuf.ToTimestamp(at),
	}, nil
}

func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	c, err := s.get(r.ID, r.ExecID)
	if err != nil {
		return nil, err
	}
	resp := &taskAPI.StateResponse{
		ID:     c.ID,
		Bundle: c.Bundle,
		Pid:    uint32(c.Pid()),
	}
	status, exit := c.Status(), c.Exit
	if r.ExecID != "" {
		e, err := s.getExec(r.ID, r.ExecID)
		if err != nil {
			return nil, err
		}
		resp.ExecID = e.ID
		resp.Pid = uint32(e.Pid())
		resp.Terminal = e.Spec().Terminal
		status, exit = e.Status(), e.Exit
	}
	switch status {
	case container.Created:
		resp.Status = apitask.Status_CREATED
	case container.Running:
		resp.Status = apitask.Status_RUNNING
	case container.Paused:
		resp.Status = apitask.Status_PAUSED
	case container.Stopped:
		resp.Status = apitask.Status_STOPPED
		code, at := exit()
		resp.ExitStatus = code
		resp.ExitedAt = protobuf.ToTimestamp(at)
	}
	return resp, nil
}

func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	if r.ExecID != "" {
		e, err := s.getExec(r.ID, r.ExecID)
		if err != nil {
			return nil, err
		}
		if err := e.Kill(syscall.Signal(r.Signal)); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
		return empty, nil
	}
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	if err := c.Kill(syscall.Signal(r.Signal), r.All); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return empty, nil
}

func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	pids, err := c.Pids()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	resp := &taskAPI.PidsResponse{}
	for _, pid := range pids {
		resp.Processes = append(resp.Processes, &apitask.ProcessInfo{Pid: uint32(pid)})
	}
	return resp, nil
}

func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	opts := container.CheckpointOpts{ImagePath: r.Path}
	if r.Options != nil {
		v, err := typeurl.UnmarshalAny(r.Options)
		if err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
		if o, ok := v.(*options.CheckpointOptions); ok {
			opts.Exit = o.Exit
			opts.OpenTCP = o.OpenTcp
			opts.UnixSockets = o.ExternalUnixSockets
			opts.FileLocks = o.FileLocks
			opts.WorkPath = o.WorkPath
			if o.ImagePath != "" {
				opts.ImagePath = o.ImagePath
			}
		}
	}
	if err := c.Checkpoint(ctx, opts); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	s.send(&eventstypes.TaskCheckpointed{ContainerID: c.ID})
	return empty, nil
}

func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	c, err := s.get(r.ID, r.ExecID)
	if err != nil {
		return nil, err
	}
	exited, exit := c.Exited(), c.Exit
	if r.ExecID != "" {
		e, err := s.getExec(r.ID, r.ExecID)
		if err != nil {
			return nil, err
		}
		exited, exit = e.Exited(), e.Exit
	}
	select {
	case <-exited:
	case <-ctx.Done():
		return nil, errgrpc.ToGRPC(ctx.Err())
	}
	status, at := exit()
	return &taskAPI.WaitResponse{ExitStatus: status, ExitedAt: protobuf.ToTimestamp(at)}, nil
}

func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	resp := &taskAPI.ConnectResponse{ShimPid: uint32(os.Getpid())}
	if c, err := s.get(r.ID, ""); err == nil {
		resp.TaskPid = uint32(c.Pid())
	}
	return resp, nil
}

func (s *service) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*ptypes.Empty, error) {
	s.mu.Lock()
	n := len(s.containers)
	s.mu.Unlock()
	if n == 0 {
		s.shutdown.Shutdown()
	}
	return empty, nil
}

func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	v, err := typeurl.UnmarshalAny(r.Spec)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	spec, ok := v.(*specs.Process)
	if !ok {
		return nil, errgrpc.ToGRPCf(errdefs.ErrInvalidArgument, "exec spec is %T", v)
	}
	spec.Terminal = r.Terminal
	_, err = c.AddExec(ctx, r.ExecID, spec, r.Stdin, r.Stdout, r.Stderr, func(e *container.Exec) {
		code, at := e.Exit()
		s.send(&eventstypes.TaskExit{
			ContainerID: r.ID,
			ID:          e.ID,
			Pid:         uint32(e.Pid()),
			ExitStatus:  code,
			ExitedAt:    protobuf.ToTimestamp(at),
		})
	})
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	s.send(&eventstypes.TaskExecAdded{ContainerID: r.ID, ExecID: r.ExecID})
	return empty, nil
}

func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	if r.ExecID != "" {
		e, err := s.getExec(r.ID, r.ExecID)
		if err != nil {
			return nil, err
		}
		if err := e.Resize(r.Width, r.Height); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
		return empty, nil
	}
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	if err := c.Resize(r.Width, r.Height); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return empty, nil
}

func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	if !r.Stdin {
		return empty, nil
	}
	if r.ExecID != "" {
		e, err := s.getExec(r.ID, r.ExecID)
		if err != nil {
			return nil, err
		}
		e.CloseStdin()
		return empty, nil
	}
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	c.CloseStdin()
	return empty, nil
}

func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	if err := c.Pause(); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	s.send(&eventstypes.TaskPaused{ContainerID: c.ID})
	return empty, nil
}

func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	if err := c.Resume(); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	s.send(&eventstypes.TaskResumed{ContainerID: c.ID})
	return empty, nil
}

func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	v, err := typeurl.UnmarshalAny(r.Resources)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	res, ok := v.(*specs.LinuxResources)
	if !ok {
		return nil, errgrpc.ToGRPCf(errdefs.ErrInvalidArgument, "resources are %T", v)
	}
	if err := c.Update(res); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return empty, nil
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	c, err := s.get(r.ID, "")
	if err != nil {
		return nil, err
	}
	m, err := c.Stats()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	a, err := typeurl.MarshalAnyToProto(m)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &taskAPI.StatsResponse{Stats: a}, nil
}

// migrateOut sends container c to the receiver at target.
func (s *service) migrateOut(ctx context.Context, c *container.Container, ex *extras, target, token string) (migrate.Result, error) {
	if !c.Running() {
		return migrate.Result{}, fmt.Errorf("container is not running: %w", errdefs.ErrFailedPrecondition)
	}
	if err := c.SetMigrating(true); err != nil {
		return migrate.Result{}, err
	}
	defer c.SetMigrating(false)
	work := filepath.Join(ex.stateDir, "migrate-out")
	_ = os.RemoveAll(work)
	res, err := migrate.Send(ctx, c, target, token, work)
	if err == nil {
		// Kept on failure: it holds the CRIU logs.
		_ = os.RemoveAll(work)
	}
	return res, err
}

// getExec returns the exec process execID of container id.
func (s *service) getExec(id, execID string) (*container.Exec, error) {
	c, err := s.get(id, "")
	if err != nil {
		return nil, err
	}
	e, err := c.Exec(execID)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return e, nil
}

// get returns container id; with an exec id it only checks the exec exists.
func (s *service) get(id, execID string) (*container.Container, error) {
	s.mu.Lock()
	c, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "container %s", id)
	}
	if execID != "" {
		if _, err := c.Exec(execID); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
	}
	return c, nil
}

func (s *service) handleExit(c *container.Container) {
	status, at := c.Exit()
	s.send(&eventstypes.TaskExit{
		ContainerID: c.ID,
		ID:          c.ID,
		Pid:         uint32(c.Pid()),
		ExitStatus:  status,
		ExitedAt:    protobuf.ToTimestamp(at),
	})
}

// send publishes an event to containerd. The shim's context carries the
// containerd namespace the shim was started for.
func (s *service) send(evt any) {
	if err := s.publisher.Publish(s.ctx, runtime.GetTopic(evt), evt); err != nil {
		log.G(s.ctx).WithError(err).Errorf("publish %T", evt)
	}
}

// waitKillable reports whether libseccomp can set
// SECCOMP_FILTER_FLAG_WAIT_KILLABLE_RECV: 2.6.0, API level 7.
var waitKillable = sync.OnceValue(func() bool {
	major, minor, _ := seccomp.Version()
	return major > 2 || (major == 2 && minor >= 6)
})

// coverCgroup puts the container's cgroup under the netlink policy, if it
// has one and the policy is loaded. It is called at create and again at
// start, since a restored container's cgroup appears only then; covering
// an already covered one does nothing.
func (e *extras) coverCgroup(c *container.Container) error {
	if e.netPolicy == nil || e.cgroupID != 0 {
		return nil
	}
	dir, err := c.CgroupPath()
	if err != nil {
		return err
	}
	id, err := netpolicy.CgroupID(dir)
	if err != nil {
		return fmt.Errorf("cgroup %s: %w", dir, err)
	}
	if err := e.netPolicy.Add(id); err != nil {
		return err
	}
	e.cgroupID = id
	return nil
}

// forkSource reads the fork annotation: the snapshot directory a new
// container starts from, and the allocator key of the user namespace
// range its container mapped (empty when the snapshot had none).
func forkSource(spec *specs.Spec) (dir, key string, err error) {
	dir = spec.Annotations[container.AnnotationForkFrom]
	if dir == "" {
		return "", "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", "", fmt.Errorf("%s=%q must be an absolute path: %w",
			container.AnnotationForkFrom, dir, errdefs.ErrInvalidArgument)
	}
	for _, part := range []string{"images", "upper"} {
		if _, err := os.Stat(filepath.Join(dir, part)); err != nil {
			return "", "", fmt.Errorf("%s=%q is not a snapshot: %w",
				container.AnnotationForkFrom, dir, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, userns.KeyFile))
	if errors.Is(err, os.ErrNotExist) {
		return dir, "", nil // a snapshot of a container without one
	}
	if err != nil {
		return "", "", err
	}
	return dir, strings.TrimSpace(string(b)), nil
}
