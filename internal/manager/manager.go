//go:build linux

// Package manager starts and stops shim processes for containerd.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/opencontainers/runc/libcontainer"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/container"
)

func New(name string) shim.Shim {
	return &manager{name: name}
}

type manager struct {
	name string
}

func (m *manager) Name() string {
	return m.name
}

// Start launches a shim process serving one container and returns its address.
// containerd runs this with the bundle as working directory.
func (m *manager) Start(ctx context.Context, opts *bootapi.BootstrapParams) (_ *bootapi.BootstrapResult, retErr error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	id := opts.GetInstanceID()
	result := &bootapi.BootstrapResult{Version: 3, Protocol: "ttrpc"}

	socketDir := opts.GetSocketDir()
	if socketDir == "" {
		socketDir = filepath.Join(defaults.DefaultStateDir, "s")
	}
	address, err := shim.CreateSocketAddress(ctx, socketDir, opts.GetContainerdGrpcAddress(), id, false)
	if err != nil {
		return nil, err
	}
	socket, err := shim.NewSocket(address)
	if err != nil {
		if !shim.SocketEaddrinuse(err) {
			return nil, fmt.Errorf("create shim socket: %w", err)
		}
		if shim.CanConnect(address) {
			result.Address = address
			return result, nil
		}
		if err := shim.RemoveSocket(address); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
		if socket, err = shim.NewSocket(address); err != nil {
			return nil, fmt.Errorf("create shim socket: %w", err)
		}
	}
	defer func() {
		if retErr != nil {
			socket.Close()
			_ = shim.RemoveSocket(address)
		}
	}()
	f, err := socket.File()
	if err != nil {
		return nil, err
	}
	defer f.Close()

	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	args := []string{"-namespace", ns, "-id", id, "-address", opts.GetContainerdGrpcAddress()}
	if opts.GetLogLevel() <= bootapi.LogLevel_LOG_LEVEL_DEBUG {
		args = append(args, "-debug")
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=4")
	cmd.ExtraFiles = []*os.File{f}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait()
	if err := shim.AdjustOOMScore(cmd.Process.Pid); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("adjust shim oom score: %w", err)
	}

	result.Address = address
	return result, nil
}

// Stop cleans up after a shim that is gone: kills what is left of the
// container, removes its libcontainer state and unmounts its rootfs.
func (m *manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return shim.StopStatus{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return shim.StopStatus{}, err
	}
	bundle := filepath.Join(filepath.Dir(cwd), id)

	pid := 0
	lc, err := libcontainer.Load(container.Root(ns), id)
	switch {
	case err == nil:
		if st, err := lc.State(); err == nil {
			pid = st.InitProcessPid
		}
		_ = lc.Signal(unix.SIGKILL)
		if err := lc.Destroy(); err != nil {
			return shim.StopStatus{}, fmt.Errorf("destroy container: %w", err)
		}
	case errors.Is(err, libcontainer.ErrNotExist):
	default:
		return shim.StopStatus{}, err
	}
	if err := mount.UnmountRecursive(filepath.Join(bundle, "rootfs"), 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return shim.StopStatus{}, fmt.Errorf("unmount rootfs: %w", err)
	}
	return shim.StopStatus{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + int(unix.SIGKILL),
		Pid:        pid,
	}, nil
}

func (m *manager) Info(ctx context.Context, optionsR io.Reader) (*types.RuntimeInfo, error) {
	return &types.RuntimeInfo{
		Name:    m.name,
		Version: &types.RuntimeVersion{Version: "dev"},
	}, nil
}
