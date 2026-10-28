//go:build linux

// Package task implements containerd's task API on top of internal/container.
package task

import (
	"context"
	"os"
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

	"github.com/makhov/gcr/internal/container"
)

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
}

func NewTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
	s := &service{
		ctx:        context.WithoutCancel(ctx),
		publisher:  publisher,
		shutdown:   sd,
		containers: make(map[string]*container.Container),
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

	c, err := container.Create(ctx, container.CreateOpts{
		ID:         r.ID,
		Namespace:  ns,
		Bundle:     r.Bundle,
		Rootfs:     rootfs,
		Stdin:      r.Stdin,
		Stdout:     r.Stdout,
		Stderr:     r.Stderr,
		Checkpoint: r.Checkpoint,
	}, s.handleExit)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	s.mu.Lock()
	s.containers[r.ID] = c
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
	if err := c.Start(ctx); err != nil {
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
	s.mu.Unlock()

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
