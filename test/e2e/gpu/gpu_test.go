//go:build e2e && linux

// Package gpu checks containers with NVIDIA GPUs. TestCUDA needs a GPU
// node: the NVIDIA driver, nvidia-ctk's CDI spec (/etc/cdi/nvidia.yaml)
// and CRIU's CUDA plugin; it is skipped elsewhere. TestNoDriver runs
// anywhere.
package gpu

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cdi"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

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

// cudaImage runs PyTorch; GCR_E2E_CUDA_IMAGE overrides it.
func cudaImage() string {
	if img := os.Getenv("GCR_E2E_CUDA_IMAGE"); img != "" {
		return img
	}
	return "docker.io/pytorch/pytorch:2.7.1-cuda12.6-cudnn9-runtime"
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
