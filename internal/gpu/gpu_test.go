//go:build linux

package gpu

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// fakeDriver lays out /proc/driver/nvidia as the driver does, with one GPU
// per minor.
func fakeDriver(t *testing.T, version string, models map[int]string) string {
	root := t.TempDir()
	proc := filepath.Join(root, "proc/driver/nvidia")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	line := "NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  " + version + "  Release Build  (dvs-builder@U16-I3-B03-4-3)  Thu Jan 30 2025\n" +
		"GCC version:  gcc version 12.3.0 (Ubuntu 12.3.0-1ubuntu1~22.04)\n"
	if err := os.WriteFile(filepath.Join(proc, "version"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	for minor, model := range models {
		dir := filepath.Join(proc, "gpus", "0000:"+string(rune('a'+minor))+"0:00.0")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		info := "Model: \t\t " + model + "\n" +
			"IRQ:   \t\t 182\n" +
			"GPU UUID: \t GPU-0000000" + string(rune('0'+minor)) + "-aaaa-bbbb-cccc-dddddddddddd\n" +
			"Video BIOS: \t 92.00.36.00.10\n" +
			"Bus Type: \t PCIe\n" +
			"DMA Size: \t 47 bits\n" +
			"Device Minor: \t " + string(rune('0'+minor)) + "\n" +
			"GPU Excluded:\t No\n"
		if err := os.WriteFile(filepath.Join(dir, "information"), []byte(info), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestMinors(t *testing.T) {
	spec := &specs.Spec{Linux: &specs.Linux{Devices: []specs.LinuxDevice{
		{Path: "/dev/nvidiactl"}, {Path: "/dev/nvidia3"}, {Path: "/dev/nvidia-uvm"},
		{Path: "/dev/nvidia1"}, {Path: "/dev/nvidia-caps/nvidia-cap1"}, {Path: "/dev/null"},
	}}}
	if got := Minors(spec); !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("Minors = %v, want [1 3]", got)
	}
	if got := Minors(&specs.Spec{}); got != nil {
		t.Fatalf("Minors of a spec without devices = %v", got)
	}
}

func TestProbe(t *testing.T) {
	root := fakeDriver(t, "570.86.15", map[int]string{0: "NVIDIA A100-SXM4-80GB", 1: "NVIDIA A100-SXM4-80GB"})
	info, err := Probe(root, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if info.Driver != "570.86.15" || len(info.GPUs) != 1 {
		t.Fatalf("info %+v", info)
	}
	if g := info.GPUs[0]; g.Minor != 1 || g.Model != "NVIDIA A100-SXM4-80GB" || g.UUID != "GPU-00000001-aaaa-bbbb-cccc-dddddddddddd" || g.Bus == "" {
		t.Fatalf("gpu %+v", g)
	}
	if _, err := Probe(root, []int{7}); err == nil {
		t.Fatal("probed a GPU the driver does not have")
	}
	if _, err := Probe(t.TempDir(), []int{0}); err == nil {
		t.Fatal("probed without a driver")
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	if info, err := Load(dir); info != nil || err != nil {
		t.Fatalf("Load without a file = %v, %v", info, err)
	}
	want := &Info{Driver: "570.86.15", GPUs: []GPU{{Minor: 0, UUID: "GPU-x", Model: "NVIDIA L4"}}}
	if err := want.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil || got.Driver != want.Driver || len(got.GPUs) != 1 || got.GPUs[0] != want.GPUs[0] {
		t.Fatalf("Load = %+v, %v", got, err)
	}
}

func TestCompatible(t *testing.T) {
	l4 := func(minor int, uuid string) GPU { return GPU{Minor: minor, UUID: uuid, Model: "NVIDIA L4"} }
	saved := &Info{Driver: "570.86.15", GPUs: []GPU{l4(0, "GPU-a")}}
	for _, tc := range []struct {
		name string
		now  *Info
		want error // nil, ErrFailedPrecondition or ErrNotImplemented
	}{
		{"same GPU", &Info{Driver: "570.86.15", GPUs: []GPU{l4(0, "GPU-a")}}, nil},
		{"another node, same index", &Info{Driver: "570.86.15", GPUs: []GPU{l4(0, "GPU-b")}}, nil},
		{"other driver", &Info{Driver: "575.51.03", GPUs: []GPU{l4(0, "GPU-a")}}, errdefs.ErrFailedPrecondition},
		{"other model", &Info{Driver: "570.86.15", GPUs: []GPU{{Minor: 0, Model: "NVIDIA H100"}}}, errdefs.ErrFailedPrecondition},
		{"two GPUs", &Info{Driver: "570.86.15", GPUs: []GPU{l4(0, "GPU-a"), l4(1, "GPU-c")}}, errdefs.ErrFailedPrecondition},
		{"no GPU", nil, errdefs.ErrFailedPrecondition},
		{"other index", &Info{Driver: "570.86.15", GPUs: []GPU{l4(2, "GPU-a")}}, errdefs.ErrNotImplemented},
	} {
		err := Compatible(saved, tc.now)
		if (tc.want == nil) != (err == nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: Compatible = %v, want %v", tc.name, err, tc.want)
		}
	}
	if err := Compatible(nil, &Info{Driver: "570.86.15"}); err != nil {
		t.Errorf("CPU checkpoint into a GPU container: %v", err)
	}
}
