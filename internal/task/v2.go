//go:build linux

package task

import (
	"context"

	taskv2 "github.com/containerd/containerd/api/runtime/task/v2"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"google.golang.org/protobuf/proto"
)

// v2 serves the task API as containerd.task.v2.Task as well, for the
// callers that speak only that: containerd before 2.0, and CRI-O's VM
// runtime type. Its messages are v3's under another package name (the
// two shim.protos differ in nothing else), so each call is passed on
// as the same bytes.
type v2 struct{ s taskAPI.TTRPCTaskService }

// call passes in to f, re-read as f's request type, and its answer back
// as Out.
func call[In, Out, In3, Out3 proto.Message](ctx context.Context, in In, f func(context.Context, In3) (Out3, error), in3 In3, out Out) (Out, error) {
	b, err := proto.Marshal(in)
	if err != nil {
		return out, err
	}
	if err := proto.Unmarshal(b, in3); err != nil {
		return out, err
	}
	res, err := f(ctx, in3)
	if err != nil {
		return out, err
	}
	if b, err = proto.Marshal(res); err != nil {
		return out, err
	}
	return out, proto.Unmarshal(b, out)
}

func (v v2) State(ctx context.Context, r *taskv2.StateRequest) (*taskv2.StateResponse, error) {
	return call(ctx, r, v.s.State, &taskAPI.StateRequest{}, &taskv2.StateResponse{})
}

func (v v2) Create(ctx context.Context, r *taskv2.CreateTaskRequest) (*taskv2.CreateTaskResponse, error) {
	return call(ctx, r, v.s.Create, &taskAPI.CreateTaskRequest{}, &taskv2.CreateTaskResponse{})
}

func (v v2) Start(ctx context.Context, r *taskv2.StartRequest) (*taskv2.StartResponse, error) {
	return call(ctx, r, v.s.Start, &taskAPI.StartRequest{}, &taskv2.StartResponse{})
}

func (v v2) Delete(ctx context.Context, r *taskv2.DeleteRequest) (*taskv2.DeleteResponse, error) {
	return call(ctx, r, v.s.Delete, &taskAPI.DeleteRequest{}, &taskv2.DeleteResponse{})
}

func (v v2) Pids(ctx context.Context, r *taskv2.PidsRequest) (*taskv2.PidsResponse, error) {
	return call(ctx, r, v.s.Pids, &taskAPI.PidsRequest{}, &taskv2.PidsResponse{})
}

func (v v2) Pause(ctx context.Context, r *taskv2.PauseRequest) (*ptypes.Empty, error) {
	return call(ctx, r, v.s.Pause, &taskAPI.PauseRequest{}, &ptypes.Empty{})
}

func (v v2) Resume(ctx context.Context, r *taskv2.ResumeRequest) (*ptypes.Empty, error) {
	return call(ctx, r, v.s.Resume, &taskAPI.ResumeRequest{}, &ptypes.Empty{})
}

func (v v2) Checkpoint(ctx context.Context, r *taskv2.CheckpointTaskRequest) (*ptypes.Empty, error) {
	return call(ctx, r, v.s.Checkpoint, &taskAPI.CheckpointTaskRequest{}, &ptypes.Empty{})
}

func (v v2) Kill(ctx context.Context, r *taskv2.KillRequest) (*ptypes.Empty, error) {
	return call(ctx, r, v.s.Kill, &taskAPI.KillRequest{}, &ptypes.Empty{})
}

func (v v2) Exec(ctx context.Context, r *taskv2.ExecProcessRequest) (*ptypes.Empty, error) {
	return call(ctx, r, v.s.Exec, &taskAPI.ExecProcessRequest{}, &ptypes.Empty{})
}

func (v v2) ResizePty(ctx context.Context, r *taskv2.ResizePtyRequest) (*ptypes.Empty, error) {
	return call(ctx, r, v.s.ResizePty, &taskAPI.ResizePtyRequest{}, &ptypes.Empty{})
}

func (v v2) CloseIO(ctx context.Context, r *taskv2.CloseIORequest) (*ptypes.Empty, error) {
	return call(ctx, r, v.s.CloseIO, &taskAPI.CloseIORequest{}, &ptypes.Empty{})
}

func (v v2) Update(ctx context.Context, r *taskv2.UpdateTaskRequest) (*ptypes.Empty, error) {
	return call(ctx, r, v.s.Update, &taskAPI.UpdateTaskRequest{}, &ptypes.Empty{})
}

func (v v2) Wait(ctx context.Context, r *taskv2.WaitRequest) (*taskv2.WaitResponse, error) {
	return call(ctx, r, v.s.Wait, &taskAPI.WaitRequest{}, &taskv2.WaitResponse{})
}

func (v v2) Stats(ctx context.Context, r *taskv2.StatsRequest) (*taskv2.StatsResponse, error) {
	return call(ctx, r, v.s.Stats, &taskAPI.StatsRequest{}, &taskv2.StatsResponse{})
}

func (v v2) Connect(ctx context.Context, r *taskv2.ConnectRequest) (*taskv2.ConnectResponse, error) {
	return call(ctx, r, v.s.Connect, &taskAPI.ConnectRequest{}, &taskv2.ConnectResponse{})
}

func (v v2) Shutdown(ctx context.Context, r *taskv2.ShutdownRequest) (*ptypes.Empty, error) {
	return call(ctx, r, v.s.Shutdown, &taskAPI.ShutdownRequest{}, &ptypes.Empty{})
}
