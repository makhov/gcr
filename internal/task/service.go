//go:build linux

// Package task implements containerd's task API on top of internal/container.
package task

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/timetravel"
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
}

// extras is what a container has beyond containerd's task API.
type extras struct {
	recorder *timetravel.Recorder // time travel, if enabled
	receiver *migrate.Receiver    // incoming migration, if requested
	control  *control.Server
	stateDir string
}

func (e *extras) Close() {
	if e.control != nil {
		e.control.Close()
	}
	if e.receiver != nil {
		e.receiver.Close()
	}
	if e.recorder != nil {
		_ = e.recorder.Close()
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

	ex := &extras{stateDir: filepath.Join(stateRoot, ns, r.ID)}
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
	opts.PrepareSpec = func(spec *specs.Spec) (container.Prepared, error) {
		var prep container.Prepared
		if addr, ok := spec.Annotations[migrate.AnnotationIn]; ok {
			recv, err := migrate.NewReceiver(s.ctx, addr, spec.Annotations[migrate.AnnotationToken],
				filepath.Join(ex.stateDir, "migrate-in"),
				func() (string, error) { return c.UpperDir() })
			if err != nil {
				return prep, fmt.Errorf("migration receiver: %w", err)
			}
			ex.receiver = recv
			prep.CheckpointFrom = recv.Wait
		}
		policy, err := timetravel.ParsePolicy(spec.Annotations)
		if err != nil || policy == nil {
			return prep, err
		}
		rec, err := timetravel.NewRecorder(s.ctx, policy, filepath.Join(ex.stateDir, "time-travel"),
			strings.TrimSuffix(control.SocketPath(ns, r.ID), ".sock")+".tt")
		if err != nil {
			return prep, fmt.Errorf("time travel: %w", err)
		}
		ex.recorder = rec
		policy.AddTrigger(spec, rec.HandoffPath())
		prep.CRIUConfig = rec.CRIUConfig()
		return prep, nil
	}
	c, err = container.Create(ctx, opts, s.handleExit)
	if err != nil {
		ex.Close()
		return nil, errgrpc.ToGRPC(err)
	}

	h := control.Handlers{
		Migrate: func(ctx context.Context, target, token string) (migrate.Result, error) {
			return s.migrateOut(ctx, c, ex, target, token)
		},
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
	c, err := s.get(r.ID, r.ExecID)
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
	s.send(&eventstypes.TaskStart{ContainerID: c.ID, Pid: uint32(c.Pid())})
	return &taskAPI.StartResponse{Pid: uint32(c.Pid())}, nil
}

func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	c, err := s.get(r.ID, r.ExecID)
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
	switch c.Status() {
	case container.Created:
		resp.Status = apitask.Status_CREATED
	case container.Running:
		resp.Status = apitask.Status_RUNNING
	case container.Paused:
		resp.Status = apitask.Status_PAUSED
	case container.Stopped:
		resp.Status = apitask.Status_STOPPED
		status, at := c.Exit()
		resp.ExitStatus = status
		resp.ExitedAt = protobuf.ToTimestamp(at)
	}
	return resp, nil
}

func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	c, err := s.get(r.ID, r.ExecID)
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
	select {
	case <-c.Exited():
	case <-ctx.Done():
		return nil, errgrpc.ToGRPC(ctx.Err())
	}
	status, at := c.Exit()
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
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "exec")
}

func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "terminal")
}

func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "close io")
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
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "update")
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "stats")
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

func (s *service) get(id, execID string) (*container.Container, error) {
	if execID != "" {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "exec processes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.containers[id]
	if !ok {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "container %s", id)
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
