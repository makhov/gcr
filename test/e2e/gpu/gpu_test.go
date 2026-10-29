//go:build e2e && linux

// Package gpu checks containers with NVIDIA GPUs. TestCUDA needs a GPU
// node: the NVIDIA driver, nvidia-ctk's CDI spec (/etc/cdi/nvidia.yaml)
// and CRIU's CUDA plugin; it is skipped elsewhere. TestNoDriver runs
// anywhere.
package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cdi"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/userns"
	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

// TestNoDriver: a container whose GPUs gcr cannot describe (here a
// /dev/nvidia7 without an NVIDIA driver on the host) is refused
// checkpoints, rather than checkpointed into something no node can
// restore.
func TestNoDriver(t *testing.T) {
	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		t.Skip("host has an NVIDIA driver")
	}
	ns := framework.NewNamespace(t, "gcr-e2e")
	// Host users: in a user namespace, device nodes are bind mounts of
	// the host's, and this host has no /dev/nvidia7.
	c := ns.Run(framework.UniqueID("nogpu"), framework.Spec{
		Args:        []string{"sleep", "3600"},
		Annotations: map[string]string{userns.AnnotationHostUsers: "true"},
		SpecOpts: []oci.SpecOpts{func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
			// /dev/null's numbers at a GPU's path.
			s.Linux.Devices = append(s.Linux.Devices, specs.LinuxDevice{Path: "/dev/nvidia7", Type: "c", Major: 1, Minor: 3})
			return nil
		}},
	})
	_, err := c.Checkpoint()
	if err == nil || !strings.Contains(err.Error(), "NVIDIA driver") {
		t.Fatalf("checkpoint of a container with an undescribed GPU: %v", err)
	}
	t.Logf("refused: %v", err)
}

// cudaImage runs PyTorch; GCR_E2E_CUDA_IMAGE overrides it. Built for
// CUDA 12.8, the first with kernels for Blackwell (sm_120) as well as the
// GPUs before it.
func cudaImage() string {
	if img := os.Getenv("GCR_E2E_CUDA_IMAGE"); img != "" {
		return img
	}
	return "docker.io/pytorch/pytorch:2.7.1-cuda12.8-cudnn9-runtime"
}

// counter keeps a tensor in GPU memory and adds to it every 0.2s, writing
// its value: after a restore it must carry on, which it can only do if the
// GPU memory came back.
const counter = `
import time, torch
x = torch.zeros(1 << 20, device="cuda")
while True:
    x += 1
    torch.cuda.synchronize()
    open("/tmp/count", "w").write(str(int(x[0].item())))
    time.sleep(0.2)
`

func TestCUDA(t *testing.T) {
	if _, err := os.Stat("/dev/nvidiactl"); err != nil {
		t.Skip("no NVIDIA GPU")
	}
	ns := framework.NewNamespace(t, "gcr-e2e")
	c := ns.Run(framework.UniqueID("cuda"), framework.Spec{
		Image:    cudaImage(),
		Args:     []string{"python3", "-c", counter},
		SpecOpts: []oci.SpecOpts{cdi.WithCDIDevices("nvidia.com/gpu=0")},
	})
	var before int
	framework.Eventually(t, 3*time.Minute, "the counter to start", func() bool {
		s, _ := c.ReadFile("/tmp/count")
		before, _ = strconv.Atoi(s)
		return before > 10
	})
	c.CheckpointRestore()
	time.Sleep(2 * time.Second)
	after := c.ReadInt("/tmp/count")
	t.Logf("counter %d before checkpoint, %d 2s after restore", before, after)
	if after <= before {
		t.Fatalf("counter restarted (%d -> %d): GPU memory was not restored", before, after)
	}
}

// model holds 4 GiB of weights in GPU memory and the sum they had when
// loaded, and writes "count sum-now sum-when-loaded" every 0.2s: a fork
// must carry on counting from the snapshot with its weights intact.
const model = `
import time, torch
torch.manual_seed(0)
w = torch.randn(1 << 30, device="cuda")
loaded = w.sum().item()
n = 0
while True:
    n += 1
    open("/tmp/state.new", "w").write(f"{n} {w.sum().item()} {loaded}")
    import os; os.replace("/tmp/state.new", "/tmp/state")
    time.sleep(0.2)
`

type modelState struct {
	count       int
	sum, loaded string
}

func readModel(c *framework.Container) (modelState, bool) {
	s, err := c.ReadFile("/tmp/state")
	if err != nil {
		return modelState{}, false
	}
	var m modelState
	if _, err := fmt.Sscan(s, &m.count, &m.sum, &m.loaded); err != nil {
		return modelState{}, false
	}
	return m, true
}

// TestCUDAFork forks a container with a model in GPU memory: each fork
// gets its own copy of the device memory, restored from the snapshot,
// and the parent keeps running.
func TestCUDAFork(t *testing.T) {
	if _, err := os.Stat("/dev/nvidiactl"); err != nil {
		t.Skip("no NVIDIA GPU")
	}
	ns := framework.NewNamespace(t, "gcr-e2e")
	spec := framework.Spec{
		Image:    cudaImage(),
		Args:     []string{"python3", "-c", model},
		SpecOpts: []oci.SpecOpts{cdi.WithCDIDevices("nvidia.com/gpu=0")},
	}
	parent := ns.Run(framework.UniqueID("model"), spec)
	var at modelState
	framework.Eventually(t, 3*time.Minute, "the model to load", func() bool {
		m, ok := readModel(parent)
		at = m
		return ok && m.count > 3
	})
	dir := filepath.Join(framework.TempDir(t), "snapshot")
	begin := time.Now()
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), dir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Logf("snapshot of a 4 GiB model in GPU memory in %s", time.Since(begin).Round(time.Millisecond))

	const forks = 2
	children := make([]*framework.Container, forks)
	for i := range children {
		s := spec
		s.Annotations = map[string]string{container.AnnotationForkFrom: dir}
		begin := time.Now()
		children[i] = ns.Run(framework.UniqueID(fmt.Sprintf("model-fork%d", i)), s)
		t.Logf("fork %d started in %s", i, time.Since(begin).Round(time.Millisecond))
	}
	for i, c := range append(children, parent) {
		var m modelState
		framework.Eventually(t, time.Minute, fmt.Sprintf("container %d to count on", i), func() bool {
			var ok bool
			m, ok = readModel(c)
			return ok && m.count > at.count+2
		})
		if m.sum != m.loaded || m.loaded != at.loaded {
			t.Fatalf("container %d: weights sum to %s, %s when loaded, %s in the parent: GPU memory came back wrong", i, m.sum, m.loaded, at.loaded)
		}
	}
}
