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
	// As CDI writes them: the name and the device numbers agree.
	spec := &specs.Spec{Linux: &specs.Linux{Devices: []specs.LinuxDevice{
		{Path: "/dev/nvidiactl", Major: 195, Minor: 255},
		{Path: "/dev/nvidia3", Major: 195, Minor: 3},
		{Path: "/dev/nvidia-uvm", Major: 234, Minor: 0},
		{Path: "/dev/nvidia1", Major: 195, Minor: 1},
		{Path: "/dev/nvidia-caps/nvidia-cap1", Major: 237, Minor: 1},
		{Path: "/dev/null", Major: 1, Minor: 3},
	}}}
	if got := Minors(spec); !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("Minors = %v, want [1 3]", got)
	}
	if got := Minors(&specs.Spec{}); got != nil {
		t.Fatalf("Minors of a spec without devices = %v", got)
	}
	// A GPU presented at another GPU's name, as a restore onto another
	// index has to: the numbers say which GPU it is, not the name.
	aliased := &specs.Spec{Linux: &specs.Linux{Devices: []specs.LinuxDevice{
		{Path: "/dev/nvidia3", Major: 195, Minor: 5},
	}}}
	if got := Minors(aliased); !slices.Equal(got, []int{5}) {
		t.Fatalf("Minors of a GPU presented at another name = %v, want [5]", got)
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
		// The index a GPU had is not what a restore binds to: the
		// plugin's checkpoint gives the devices back to the driver, so a
		// dump holds no /dev/nvidia<minor>, and the restored process
		// takes whichever GPUs its container was given.
		{"another index", &Info{Driver: "570.86.15", GPUs: []GPU{l4(2, "GPU-a")}}, nil},
		{"another index and another card", &Info{Driver: "570.86.15", GPUs: []GPU{l4(5, "GPU-b")}}, nil},
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

func TestSupported(t *testing.T) {
	for driver, ok := range map[string]bool{
		"580.65.06":  true,
		"595.91.07":  true,
		"1000.1":     true,
		"570.86.15":  false,
		"550.144.03": false,
		"":           false,
	} {
		err := Supported(driver)
		if (err == nil) != ok {
			t.Errorf("%q: %v", driver, err)
		}
		if err != nil && driver != "" && !errors.Is(err, errdefs.ErrNotImplemented) {
			t.Errorf("%q: %v is not ErrNotImplemented", driver, err)
		}
	}
}

func TestDriver(t *testing.T) {
	root := fakeDriver(t, "580.65.06", nil)
	if v, err := Driver(root); v != "580.65.06" || err != nil {
		t.Fatalf("%q, %v", v, err)
	}
	if _, err := Driver(t.TempDir()); err == nil {
		t.Fatal("no driver, no error")
	}
}

func TestLibCUDA(t *testing.T) {
	spec := &specs.Spec{Mounts: []specs.Mount{
		{Destination: "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.580.65.06", Source: "/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.580.65.06"},
		{Destination: "/usr/lib/x86_64-linux-gnu/libcuda.so.580.65.06", Source: "/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libcuda.so.580.65.06"},
	}}
	if got := LibCUDA(spec); got != "/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libcuda.so.580.65.06" {
		t.Fatalf("got %q", got)
	}
	if got := LibCUDA(&specs.Spec{}); got != "" {
		t.Fatalf("got %q without mounts", got)
	}
}

func TestUseLibCUDA(t *testing.T) {
	t.Setenv("LD_LIBRARY_PATH", "/opt/lib")
	dir := t.TempDir()
	lib := filepath.Join(t.TempDir(), "libcuda.so.580.65.06")
	for range 2 { // twice: the second changes nothing
		if err := UseLibCUDA(dir, lib); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.SplitList(os.Getenv("LD_LIBRARY_PATH"))
	if len(path) != 2 || path[1] != "/opt/lib" {
		t.Fatalf("LD_LIBRARY_PATH %v", path)
	}
	if got, err := os.Readlink(filepath.Join(path[0], "libcuda.so.1")); got != lib || err != nil {
		t.Fatalf("libcuda.so.1 -> %q, %v", got, err)
	}
	entries, _ := os.ReadDir(path[0])
	if len(entries) != 1 {
		t.Fatalf("%d entries beside libcuda.so.1", len(entries)-1)
	}
}
