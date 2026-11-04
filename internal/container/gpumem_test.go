//go:build linux

package container

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const giB = 1 << 30

func TestMemoryRoom(t *testing.T) {
	type cg struct {
		dir                      string
		max, current, file, swap string // "" leaves the file out
	}
	write := func(t *testing.T, path, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gb := func(n float64) string { return itoa(uint64(n * giB)) }
	for _, tc := range []struct {
		name       string
		cgroups    []cg
		available  float64 // GiB
		swapFree   float64
		swappiness string
		want       float64
		why        string
	}{
		{
			name:      "no limit: the node's",
			cgroups:   []cg{{dir: "/pod/ctr", max: "max", current: gb(1)}},
			available: 300, swappiness: "60",
			want: 300, why: "the node has 300.0 GiB",
		},
		{
			name:      "the container's limit",
			cgroups:   []cg{{dir: "/pod/ctr", max: gb(8), current: gb(0.5)}},
			available: 300, swappiness: "0",
			want: 7.5, why: "cgroup /pod/ctr is limited to 8.0 GiB",
		},
		{
			name: "the pod's limit is the tighter",
			cgroups: []cg{
				{dir: "/pod/ctr", max: gb(16), current: gb(2)},
				{dir: "/pod", max: gb(10), current: gb(6)},
			},
			available: 300, swappiness: "0",
			want: 4, why: "cgroup /pod is limited",
		},
		{
			name:      "page cache is room",
			cgroups:   []cg{{dir: "/pod/ctr", max: gb(8), current: gb(7), file: gb(6)}},
			available: 300, swappiness: "0",
			want: 7,
		},
		{
			name:      "swap counts where the kernel swaps",
			cgroups:   []cg{{dir: "/pod/ctr", max: gb(8), current: gb(0.5)}},
			available: 300, swapFree: 64, swappiness: "60",
			want: 71.5, why: "with 64.0 GiB of swap it may use",
		},
		{
			name:      "swap does not count at swappiness 0",
			cgroups:   []cg{{dir: "/pod/ctr", max: gb(8), current: gb(0.5)}},
			available: 300, swapFree: 64, swappiness: "0",
			want: 7.5,
		},
		{
			name:      "the cgroup's swap limit",
			cgroups:   []cg{{dir: "/pod/ctr", max: gb(8), current: gb(0.5), swap: gb(4)}},
			available: 300, swapFree: 64, swappiness: "60",
			want: 11.5,
		},
		{
			name:      "the node is short",
			cgroups:   []cg{{dir: "/pod/ctr", max: "max", current: gb(1)}},
			available: 6, swapFree: 64, swappiness: "0",
			want: 6, why: "vm.swappiness is 0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fs, proc := filepath.Join(root, "cgroup"), filepath.Join(root, "proc")
			for _, c := range tc.cgroups {
				d := filepath.Join(fs, c.dir)
				write(t, filepath.Join(d, "memory.max"), c.max+"\n")
				write(t, filepath.Join(d, "memory.current"), c.current+"\n")
				file := c.file
				if file == "" {
					file = "0"
				}
				write(t, filepath.Join(d, "memory.stat"), "anon 1\nfile "+file+"\n")
				if c.swap != "" {
					write(t, filepath.Join(d, "memory.swap.max"), c.swap+"\n")
					write(t, filepath.Join(d, "memory.swap.current"), "0\n")
				}
			}
			write(t, filepath.Join(proc, "42/cgroup"), "0::"+tc.cgroups[0].dir+"\n")
			write(t, filepath.Join(proc, "meminfo"), "MemTotal:       370000000 kB\nMemAvailable:   "+itoa(uint64(tc.available*giB)>>10)+" kB\nSwapFree:       "+itoa(uint64(tc.swapFree*giB)>>10)+" kB\n")
			write(t, filepath.Join(proc, "sys/vm/swappiness"), tc.swappiness+"\n")
			r, err := memoryRoom(fs, proc, 42)
			if err != nil {
				t.Fatal(err)
			}
			if got := float64(r.bytes) / giB; got < tc.want-0.01 || got > tc.want+0.01 {
				t.Errorf("room %.2f GiB, want %.2f (%s)", got, tc.want, r.why)
			}
			if !strings.Contains(r.why, tc.why) {
				t.Errorf("why %q, want it to say %q", r.why, tc.why)
			}
		})
	}
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }
