//go:build linux

package container

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/containerd/errdefs"
	devices "github.com/opencontainers/cgroups/devices/config"
	"github.com/opencontainers/runc/libcontainer/configs"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/gpu"
)

func TestGPUMoves(t *testing.T) {
	info := func(gpus ...gpu.GPU) *gpu.Info { return &gpu.Info{GPUs: gpus} }
	g := func(minor int, bus string) gpu.GPU { return gpu.GPU{Minor: minor, Bus: bus} }
	for _, tc := range []struct {
		name       string
		saved, now *gpu.Info
		want       map[int]int
	}{
		{"none", nil, nil, nil},
		{"the same GPU", info(g(0, "0000:05:00.0")), info(g(0, "0000:05:00.0")), map[int]int{}},
		{"another GPU", info(g(0, "0000:05:00.0")), info(g(3, "0000:08:00.0")), map[int]int{0: 3}},
		// Paired in bus order, as CUDA numbers them, whatever order the
		// spec listed them in.
		{"two, listed in another order", info(g(1, "0000:06:00.0"), g(0, "0000:05:00.0")), info(g(2, "0000:07:00.0"), g(3, "0000:08:00.0")), map[int]int{0: 2, 1: 3}},
		{"another node, the same minor", info(g(0, "0000:05:00.0")), info(g(0, "0000:41:00.0")), map[int]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gpuMoves(tc.saved, tc.now); !maps.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// In a user namespace a GPU's device nodes are bind mounts from the host:
// the snapshot's go where its were, bound to this container's, and the
// mount check no longer finds them missing.
func TestGPUMounts(t *testing.T) {
	cfg := &configs.Config{
		Rootfs: t.TempDir(),
		Devices: []*devices.Device{
			{Path: "/dev/nvidiactl"}, {Path: "/dev/nvidia3"},
			{Path: "/dev/dri/card4"}, {Path: "/dev/dri/renderD131"},
		},
		Mounts: []*configs.Mount{{Destination: "/etc/hosts", Device: "bind", Flags: unix.MS_BIND}},
	}
	had := map[string]bool{"/etc/hosts": true, "/dev/nvidiactl": true, "/dev/nvidia0": true, "/dev/dri/card1": true, "/dev/dri/renderD128": true}
	if err := checkMounts(had, nil, cfg); err == nil {
		t.Fatal("the snapshot's GPU devices were not missing without the move")
	}
	opts, err := gpuMounts(map[int]int{0: 3}, had, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ext-mount-map /dev/dri/card1:/dev/dri/card4",
		"ext-mount-map /dev/dri/renderD128:/dev/dri/renderD131",
		"ext-mount-map /dev/nvidia0:/dev/nvidia3",
	}
	if !slices.Equal(opts, want) {
		t.Fatalf("got %q, want %q", opts, want)
	}
	if err := checkMounts(had, gpuMounted(opts), cfg); err != nil {
		t.Fatalf("refused: %v", err)
	}

	// GPUs that trade minors would need a mount moved onto another.
	cfg.Devices = []*devices.Device{{Path: "/dev/nvidia0"}, {Path: "/dev/nvidia1"}}
	had = map[string]bool{"/dev/nvidia0": true, "/dev/nvidia1": true}
	if _, err := gpuMounts(map[int]int{0: 1, 1: 0}, had, cfg); !errors.Is(err, errdefs.ErrNotImplemented) {
		t.Fatalf("GPUs trading minors: %v", err)
	}
}
