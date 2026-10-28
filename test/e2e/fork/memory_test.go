//go:build e2e && linux

package fork

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/test/e2e/framework"
)

// TestForkMemory checks that forks of one snapshot share their memory:
// restored from the same pages, they hold identical copies at first, and
// the kernel (KSM) merges those into one, copied again only where a fork
// writes. Ten forks must come to less than twice the memory of one.
func TestForkMemory(t *testing.T) {
	withKSM(t)
	const mib = 128
	ns := framework.NewNamespace(t, "gcr-e2e")
	hog := framework.BuildStatic(t, "hog")
	parent := ns.Run(framework.UniqueID("hog"), framework.Spec{
		Args:   []string{"/hog", strconv.Itoa(mib)},
		Mounts: []specs.Mount{framework.BindRO(hog, "/hog")},
	})
	framework.Eventually(t, 2*time.Minute, "the parent to fill its memory", func() bool {
		s, _ := parent.ReadFile("/tmp/ready")
		return s == strconv.Itoa(mib)
	})
	dir := filepath.Join(framework.TempDir(t), "snapshot")
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), dir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	children := make([]*framework.Container, forks)
	for i := range children {
		children[i] = ns.Run(framework.UniqueID(fmt.Sprintf("fork%d", i)), framework.Spec{
			Args:   []string{"/hog", strconv.Itoa(mib)},
			Mounts: []specs.Mount{framework.BindRO(hog, "/hog")},
			Annotations: map[string]string{
				container.AnnotationForkFrom:    dir,
				container.AnnotationMemoryMerge: "true",
			},
		})
	}
	// Each fork restores the snapshot's memory: unmerged, ten forks hold
	// ten copies. (Merging starts while the later ones are still being
	// restored, so the first may hold less by now.)
	one := mib
	total := func() int {
		n := 0
		for _, c := range children {
			n += anonMiB(t, c)
		}
		return n
	}
	t.Logf("%d forks of %d MiB: %d MiB once all are up", forks, one, total())
	begin := time.Now()
	framework.Eventually(t, 3*time.Minute, "the forks to share their memory", func() bool {
		return total() < 2*one
	})
	t.Logf("%d forks: %d MiB after %s (one fork: %d MiB)", forks, total(), time.Since(begin).Round(time.Second), one)

	// Shared memory is still each fork's own: they read their pattern back
	// as they count (the hog exits on a wrong page) and keep counting.
	for i, c := range children {
		at := c.ReadInt("/tmp/count")
		framework.Eventually(t, time.Minute, "the fork to count on", func() bool {
			s, _ := c.ReadFile("/tmp/count")
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("fork %d: %q", i, s)
			}
			return n > at
		})
	}
}

// withKSM turns the kernel's same-page merging on for the test, scanning
// fast enough to finish in seconds, and puts the settings back after.
func withKSM(t *testing.T) {
	t.Helper()
	const sys = "/sys/kernel/mm/ksm/"
	if _, err := os.Stat(sys + "run"); err != nil {
		t.Skipf("no KSM in this kernel: %v", err)
	}
	for k, v := range map[string]string{"run": "1", "pages_to_scan": "20000", "sleep_millisecs": "20"} {
		old, err := os.ReadFile(sys + k)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sys+k, []byte(v), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(sys+k, old, 0) })
	}
}

// anonMiB is the anonymous memory charged to a container's cgroup.
func anonMiB(t *testing.T, c *framework.Container) int {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", c.Task.Pid()))
	if err != nil {
		t.Fatal(err)
	}
	cg := strings.TrimSpace(strings.TrimPrefix(string(b), "0::"))
	stat, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cg, "memory.stat"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(stat), "\n") {
		if v, ok := strings.CutPrefix(l, "anon "); ok {
			n, _ := strconv.Atoi(v)
			return n >> 20
		}
	}
	t.Fatalf("no anon in %s's memory.stat", c.ID)
	return 0
}

// TestMemoryMergeFresh checks that a container started from its image,
// not restored, merges too: its init turns merging on before the exec.
// Two hogs of the same size fill their memory alike.
func TestMemoryMergeFresh(t *testing.T) {
	withKSM(t)
	const mib = 128
	ns := framework.NewNamespace(t, "gcr-e2e")
	hog := framework.BuildStatic(t, "hog")
	var hogs []*framework.Container
	for i := range 2 {
		c := ns.Run(framework.UniqueID(fmt.Sprintf("hog%d", i)), framework.Spec{
			Args:        []string{"/hog", strconv.Itoa(mib)},
			Mounts:      []specs.Mount{framework.BindRO(hog, "/hog")},
			Annotations: map[string]string{container.AnnotationMemoryMerge: "true"},
		})
		framework.Eventually(t, 2*time.Minute, "the hog to fill its memory", func() bool {
			s, _ := c.ReadFile("/tmp/ready")
			return s == strconv.Itoa(mib)
		})
		hogs = append(hogs, c)
	}
	framework.Eventually(t, 2*time.Minute, "the hogs to share their memory", func() bool {
		return anonMiB(t, hogs[0])+anonMiB(t, hogs[1]) < mib*3/2
	})
	t.Logf("two hogs of %d MiB: %d MiB", mib, anonMiB(t, hogs[0])+anonMiB(t, hogs[1]))
}
