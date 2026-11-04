//go:build linux

package container

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/makhov/gcr/internal/gpu"
)

// checkGPUMemory refuses a checkpoint whose GPU memory will not fit where
// the driver puts it: CRIU's CUDA plugin has the driver copy each
// process's GPU memory into its host memory before the dump, all at once,
// charged to the container's memory cgroup, and without the room the
// kernel kills the container in the middle of the checkpoint. It refuses
// only what it measures; when it cannot measure, it says so and lets the
// checkpoint go ahead.
func (c *Container) checkGPUMemory(ctx context.Context) error {
	if c.gpu == nil {
		return nil
	}
	pids, err := c.lc.Processes()
	if err == nil && len(pids) == 0 {
		return nil
	}
	var need uint64
	var r room
	if err == nil {
		need, err = gpu.MemoryUsed(c.gpuTools, pids)
	}
	if err == nil && need > 0 {
		r, err = memoryRoom("/sys/fs/cgroup", "/proc", pids[0])
	}
	if err != nil {
		log.G(ctx).WithError(err).Warn("cannot tell whether the container has room for its GPU memory; checkpointing it anyway")
		return nil
	}
	if need <= r.bytes {
		return nil
	}
	return fmt.Errorf("a checkpoint copies the container's %s of GPU memory into its memory, which has room for %s: %s; raise its memory limit, or give the node swap (docs/gpus.md, When GPU memory is larger than RAM): %w",
		gib(need), gib(r.bytes), r.why, errdefs.ErrFailedPrecondition)
}

// room is how much more memory a cgroup's processes can take, and what
// sets it.
type room struct {
	bytes uint64
	why   string
}

// memoryRoom is the room the processes of pid's cgroup have: the least the
// cgroup or any of its ancestors (a pod's) has under its memory limit, and
// what the node has available. Page cache counts as room, since the
// kernel reclaims it; so does swap, where the kernel swaps (vm.swappiness
// above 0, as in cgroup v2 a cgroup has no swappiness of its own) and a
// cgroup's memory.swap.max allows.
func memoryRoom(cgroupfs, proc string, pid int) (room, error) {
	b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return room{}, err
	}
	rel, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "0::")
	if !ok {
		return room{}, fmt.Errorf("pid %d is not in a cgroup v2 hierarchy", pid)
	}
	info, err := meminfo(filepath.Join(proc, "meminfo"))
	if err != nil {
		return room{}, err
	}
	swaps := false
	if b, err := os.ReadFile(filepath.Join(proc, "sys/vm/swappiness")); err == nil {
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		swaps = n > 0
	}
	swapFree := uint64(0)
	if swaps {
		swapFree = info["SwapFree"]
	}
	r := room{info["MemAvailable"] + swapFree, fmt.Sprintf("the node has %s of memory available", gib(info["MemAvailable"]))}
	if swapFree > 0 {
		r.why += fmt.Sprintf(" and %s of swap free", gib(swapFree))
	} else if info["SwapFree"] > 0 {
		r.why += " and swap the kernel does not use (vm.swappiness is 0)"
	}
	for dir := filepath.Clean(rel); ; dir = filepath.Dir(dir) {
		path := filepath.Join(cgroupfs, dir)
		limit, limited, err := cgroupLimit(filepath.Join(path, "memory.max"))
		if err != nil {
			return room{}, err
		}
		if limited {
			current, err := cgroupValue(filepath.Join(path, "memory.current"))
			if err != nil {
				return room{}, err
			}
			stat, err := memoryStat(filepath.Join(path, "memory.stat"))
			if err != nil {
				return room{}, err
			}
			used := current - min(current, stat["file"])
			free := limit - min(limit, used)
			swap := uint64(0)
			if swaps {
				swap = swapFree
				if max, limited, err := cgroupLimit(filepath.Join(path, "memory.swap.max")); err == nil && limited {
					cur, _ := cgroupValue(filepath.Join(path, "memory.swap.current"))
					swap = min(swap, max-min(max, cur))
				}
			}
			if free+swap < r.bytes {
				r = room{free + swap, fmt.Sprintf("cgroup %s is limited to %s and uses %s besides page cache", dir, gib(limit), gib(used))}
				if swap > 0 {
					r.why += fmt.Sprintf(", with %s of swap it may use", gib(swap))
				}
			}
		}
		if dir == "/" || dir == "." {
			return r, nil
		}
	}
}

// cgroupLimit reads a cgroup limit file: "max" is no limit; a missing
// file (the root cgroup's) is none either.
func cgroupLimit(path string) (uint64, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, false, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil, err
}

func cgroupValue(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// memoryStat reads a cgroup's memory.stat, in bytes by key.
func memoryStat(path string) (map[string]uint64, error) {
	return keyValues(path, func(k, v string) (string, uint64, bool) {
		n, err := strconv.ParseUint(v, 10, 64)
		return k, n, err == nil
	})
}

// meminfo reads /proc/meminfo, in bytes by key.
func meminfo(path string) (map[string]uint64, error) {
	return keyValues(path, func(k, v string) (string, uint64, bool) {
		n, err := strconv.ParseUint(strings.TrimSuffix(v, " kB"), 10, 64)
		return strings.TrimSuffix(k, ":"), n << 10, err == nil
	})
}

func keyValues(path string, parse func(k, v string) (string, uint64, bool)) (map[string]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		if k, n, ok := parse(k, strings.TrimSpace(v)); ok {
			m[k] = n
		}
	}
	return m, sc.Err()
}

func gib(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }
