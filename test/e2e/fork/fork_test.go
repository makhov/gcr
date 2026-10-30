//go:build e2e && linux

// Package fork checks forking: many containers started from one
// snapshot, each carrying on from the state it holds. The snapshot's
// writable layer stays underneath every fork's own, so a fork costs an
// empty layer and no copy, and what one writes no other one sees.
package fork

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

const forks = 10

// counter writes a number every 0.2s, and /parent once at the start: a
// fork must have /parent (the snapshot's layer) and a counter that goes
// on from where the snapshot was taken, not from zero.
const counter = `
echo parent > /parent
i=0
while true; do
	i=$((i+1)); echo $i > /tmp/count.new && mv /tmp/count.new /tmp/count
	sleep 0.2
done`

func TestFork(t *testing.T) { testFork(t, false) }

// withLazyRestore lets the restored tasks of a container in a user
// namespace open the userfaultfd a lazy restore needs, and puts the
// node's setting back afterwards.
func withLazyRestore(t *testing.T) {
	const knob = "/proc/sys/vm/unprivileged_userfaultfd"
	old, err := os.ReadFile(knob)
	if err != nil {
		t.Skipf("no %s: %v", knob, err)
	}
	if strings.TrimSpace(string(old)) == "1" {
		return
	}
	if err := os.WriteFile(knob, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(knob, old, 0o644) })
}

// TestSnapshotOfLazyFork snapshots a fork whose memory is still being
// filled on demand, and forks that: the memory the lazy fork never
// touched must be in its snapshot too, which the second fork checks as it
// reads its pattern back from pages all over the chunk.
func TestSnapshotOfLazyFork(t *testing.T) {
	withLazyRestore(t)
	const mib = "256"
	ns := framework.NewNamespace(t, "gcr-e2e")
	hog := framework.BuildStatic(t, "hog")
	spec := func(annotations map[string]string) framework.Spec {
		return framework.Spec{
			Args:        []string{"/hog", mib},
			Mounts:      []specs.Mount{framework.BindRO(hog, "/hog")},
			Annotations: annotations,
		}
	}
	parent := ns.Run(framework.UniqueID("hog"), spec(nil))
	framework.Eventually(t, 2*time.Minute, "the parent to fill its memory", func() bool {
		s, _ := parent.ReadFile("/tmp/ready")
		return s == mib
	})
	tmp := framework.TempDir(t)
	first := filepath.Join(tmp, "first")
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), first); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	a := ns.Run(framework.UniqueID("lazy"), spec(map[string]string{
		container.AnnotationForkFrom:    first,
		container.AnnotationLazyRestore: "true",
	}))
	framework.Eventually(t, time.Minute, "the lazy fork to count", func() bool {
		n, err := strconv.Atoi(func() string { s, _ := a.ReadFile("/tmp/count"); return s }())
		return err == nil && n > 0
	})
	second := filepath.Join(tmp, "second")
	if err := control.NewClient(ns.Name, a.ID).Snapshot(t.Context(), second); err != nil {
		t.Fatalf("snapshot of the lazy fork: %v", err)
	}
	b := ns.Run(framework.UniqueID("fork"), spec(map[string]string{container.AnnotationForkFrom: second}))
	start := b.ReadInt("/tmp/count")
	framework.Eventually(t, time.Minute, "the fork of the lazy fork to read 25 pages", func() bool {
		s, _ := b.ReadFile("/tmp/count")
		if strings.HasPrefix(s, "corrupt") {
			t.Fatalf("the fork of a lazy fork read back wrong memory: %s", s)
		}
		n, err := strconv.Atoi(s)
		return err == nil && n >= start+25
	})
}

// residentMiB is the resident memory of pid.
func residentMiB(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			kb, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
			return kb >> 10
		}
	}
	t.Fatalf("no VmRSS for pid %d", pid)
	return 0
}

// TestForkLazy forks with memory filled on demand: a criu lazy-pages
// daemon serves each page as the container touches it, so the fork runs
// before its memory is loaded. The checks are the parent's, since a fork
// that cannot read a page is a fork that cannot count.
func TestForkLazy(t *testing.T) {
	withLazyRestore(t)
	testFork(t, true)
}

// TestForkLatency snapshots a container holding a few hundred megabytes
// of touched memory and forks it both ways, to say what filling memory on
// demand is worth: an eager fork reads the snapshot's memory before it
// runs, a lazy one runs first and faults the pages in as it goes. Both
// have to come back with the memory intact, which the forked program
// checks as it reads its pattern back.
func TestForkLatency(t *testing.T) {
	withLazyRestore(t)
	const mib = "384"
	ns := framework.NewNamespace(t, "gcr-e2e")
	hog := framework.BuildStatic(t, "hog")
	parent := ns.Run(framework.UniqueID("hog"), framework.Spec{
		Args:   []string{"/hog", mib},
		Mounts: []specs.Mount{framework.BindRO(hog, "/hog")},
	})
	framework.Eventually(t, 2*time.Minute, "the parent to fill its memory", func() bool {
		s, _ := parent.ReadFile("/tmp/ready")
		return s == mib
	})
	dir := filepath.Join(framework.TempDir(t), "snapshot")
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), dir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	for _, lazy := range []bool{false, true} {
		name := map[bool]string{false: "eager", true: "lazy"}[lazy]
		t.Run(name, func(t *testing.T) {
			annotations := map[string]string{container.AnnotationForkFrom: dir}
			if lazy {
				annotations[container.AnnotationLazyRestore] = "true"
			}
			c := ns.Create(framework.UniqueID("fork-"+name), framework.Spec{
				Args:        []string{"/hog", mib},
				Mounts:      []specs.Mount{framework.BindRO(hog, "/hog")},
				Annotations: annotations,
			})
			begin := time.Now()
			if err := c.Start(); err != nil {
				t.Fatalf("%s fork: %v", name, err)
			}
			started := time.Since(begin)
			// It counts on, reading its pattern back as it goes: the
			// memory is there and correct, whenever it arrived.
			var count int
			framework.Eventually(t, time.Minute, "the fork to count", func() bool {
				s, _ := c.ReadFile("/tmp/count")
				n, err := strconv.Atoi(s)
				count = n
				return err == nil && n > 0
			})
			t.Logf("%s fork of %s MiB: start %s, counting at %d", name, mib, started.Round(time.Millisecond), count)
			if !lazy {
				return
			}
			// With a CRIU that copies pages only on faults, the fork
			// holds what it touched (a page each count), not the
			// snapshot's memory.
			out, _ := exec.Command("criu", "--help").CombinedOutput()
			if !strings.Contains(string(out), "--lazy-pages-on-demand") {
				t.Log("criu copies the rest of the memory in the background (no --lazy-pages-on-demand)")
				return
			}
			time.Sleep(3 * time.Second)
			rss := residentMiB(t, int(c.Task.Pid()))
			t.Logf("lazy fork resident after 3s: %d MiB", rss)
			if limit, _ := strconv.Atoi(mib); rss > limit/4 {
				t.Fatalf("the lazy fork holds %d MiB of the snapshot's %s: pages were copied it did not touch", rss, mib)
			}
		})
	}
}

func testFork(t *testing.T, lazy bool) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	parent := ns.Run(framework.UniqueID("parent"), framework.Spec{Args: []string{"sh", "-c", counter}})
	framework.Eventually(t, time.Minute, "the parent to start counting", func() bool {
		s, _ := parent.ReadFile("/tmp/count")
		n, err := strconv.Atoi(s)
		return err == nil && n > 5
	})

	// A snapshot the parent keeps running after.
	dir := filepath.Join(framework.TempDir(t), "snapshot")
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), dir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	at := parent.ReadInt("/tmp/count")
	if st := parent.Status(); st != containerd.Running {
		t.Fatalf("parent is %s after the snapshot", st)
	}
	t.Logf("snapshot at count %d, parent still running", at)

	// The forks start together, as a fan-out would.
	children := make([]*framework.Container, forks)
	var wg sync.WaitGroup
	errs := make([]error, forks)
	begin := time.Now()
	for i := range children {
		annotations := map[string]string{container.AnnotationForkFrom: dir}
		if lazy {
			annotations[container.AnnotationLazyRestore] = "true"
		}
		children[i] = ns.Create(framework.UniqueID(fmt.Sprintf("fork%d", i)), framework.Spec{
			Args:        []string{"sh", "-c", counter},
			Annotations: annotations,
		})
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = children[i].Start() }(i)
	}
	started := time.Since(begin)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("fork %d: %v", i, err)
		}
	}

	for i, c := range children {
		if st := c.Status(); st != containerd.Running {
			t.Fatalf("fork %d is %s", i, st)
		}
		// It came from the snapshot: the parent's file is there, and the
		// counter carries on instead of starting over.
		if s, err := c.ReadFile("/parent"); err != nil || s != "parent" {
			t.Fatalf("fork %d: /parent is %q, %v: the snapshot's layer is missing", i, s, err)
		}
		if n := c.ReadInt("/tmp/count"); n < at {
			t.Fatalf("fork %d: count %d, below the snapshot's %d: it restarted instead of resuming", i, n, at)
		}
	}
	t.Logf("%d forks running from one snapshot in %s (lazy=%v)", forks, started.Round(time.Millisecond), lazy)

	// Nothing was copied: every fork mounts the snapshot's layer itself.
	if n := mountsOf(t, filepath.Join(dir, "upper")); n != forks {
		t.Fatalf("%d overlays mount the snapshot's layer, want one a fork (%d)", n, forks)
	}

	// Each has its own writable layer: what one writes stays there, and
	// the snapshot underneath is unchanged for the others.
	for i, c := range children {
		if out, code := c.Exec("sh", "-c", fmt.Sprintf("echo fork%d > /whoami", i)); code != 0 {
			t.Fatalf("fork %d: write: %s", i, out)
		}
	}
	for i, c := range children {
		out, code := c.Exec("cat", "/whoami")
		if code != 0 || out != fmt.Sprintf("fork%d\n", i) {
			t.Fatalf("fork %d sees %q: forks share a writable layer", i, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "upper", "whoami")); err == nil {
		t.Fatal("a fork wrote into the snapshot's layer")
	}
}

// A fork of a fork mounts both snapshots' layers, newest on top: what the
// first fork wrote is there, and what it deleted from the first snapshot's
// layer is gone, though that layer still has it.
func TestForkOfFork(t *testing.T) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	parent := ns.Run(framework.UniqueID("parent"), framework.Spec{Args: []string{"sh", "-c", counter}})
	framework.Eventually(t, time.Minute, "the parent to start counting", func() bool {
		s, _ := parent.ReadFile("/tmp/count")
		n, err := strconv.Atoi(s)
		return err == nil && n > 0
	})
	first := filepath.Join(framework.TempDir(t), "first")
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), first); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	a := ns.Run(framework.UniqueID("fork-a"), framework.Spec{
		Args:        []string{"sh", "-c", counter},
		Annotations: map[string]string{container.AnnotationForkFrom: first},
	})
	if out, code := a.Exec("sh", "-c", "rm /parent && echo a > /a"); code != 0 {
		t.Fatalf("fork a: %s", out)
	}
	second := filepath.Join(framework.TempDir(t), "second")
	if err := control.NewClient(ns.Name, a.ID).Snapshot(t.Context(), second); err != nil {
		t.Fatalf("snapshot of a fork: %v", err)
	}
	at := a.ReadInt("/tmp/count")
	if b, err := os.ReadFile(filepath.Join(second, container.LayersFile)); err != nil ||
		strings.TrimSpace(string(b)) != filepath.Join(first, "upper") {
		t.Fatalf("second snapshot's layers: %q, %v; want the first's", b, err)
	}

	b := ns.Run(framework.UniqueID("fork-b"), framework.Spec{
		Args:        []string{"sh", "-c", counter},
		Annotations: map[string]string{container.AnnotationForkFrom: second},
	})
	if s, err := b.ReadFile("/a"); err != nil || s != "a" {
		t.Fatalf("fork b: /a is %q, %v: the second snapshot's layer is missing", s, err)
	}
	if out, code := b.Exec("test", "-e", "/parent"); code == 0 {
		t.Fatalf("fork b sees /parent, which fork a deleted: %s", out)
	}
	if _, err := os.Stat(filepath.Join(first, "upper", "parent")); err != nil {
		t.Fatalf("the first snapshot's layer lost /parent: %v", err)
	}
	// b's own fork file shadows the one a's layer brought along.
	if s, err := b.ReadFile(container.ForkFile); err != nil || !strings.Contains(s, `"id":"`+b.ID+`"`) {
		t.Fatalf("fork b: %s is %q, %v; want its own ID", container.ForkFile, s, err)
	}
	if n := b.ReadInt("/tmp/count"); n < at {
		t.Fatalf("fork b: count %d, below the second snapshot's %d", n, at)
	}
	// Each layer is mounted, not copied: by a and b, and by b.
	if n := mountsOf(t, filepath.Join(first, "upper")); n != 2 {
		t.Fatalf("%d overlays mount the first snapshot's layer, want 2", n)
	}
	if n := mountsOf(t, filepath.Join(second, "upper")); n != 1 {
		t.Fatalf("%d overlays mount the second snapshot's layer, want 1", n)
	}
}

// mountsOf counts the overlays that have layer among their lower layers.
func mountsOf(t *testing.T, layer string) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, " - overlay ") && (strings.Contains(l, "lowerdir="+layer+":") || strings.Contains(l, ":"+layer+":")) {
			n++
		}
	}
	return n
}

// TestForkIdentity checks that a fork can tell it is one: it finds its
// own ID in /.gcr/fork.json, and a fork that asked for a signal gets it
// once restored, with the file already in place for its handler to read.
// The parent has neither.
func TestForkIdentity(t *testing.T) {
	const script = `
		trap 'cat /.gcr/fork.json > /tmp/forked' USR2
		i=0
		while true; do
			i=$((i+1)); echo $i > /tmp/count.new && mv /tmp/count.new /tmp/count
			sleep 0.2
		done`
	ns := framework.NewNamespace(t, "gcr-e2e")
	parent := ns.Run(framework.UniqueID("parent"), framework.Spec{Args: []string{"sh", "-c", script}})
	framework.Eventually(t, time.Minute, "the parent to start counting", func() bool {
		_, err := parent.ReadFile("/tmp/count")
		return err == nil
	})
	dir := filepath.Join(framework.TempDir(t), "snapshot")
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), dir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	quiet := ns.Run(framework.UniqueID("fork-quiet"), framework.Spec{
		Args:        []string{"sh", "-c", script},
		Annotations: map[string]string{container.AnnotationForkFrom: dir},
	})
	signalled := ns.Run(framework.UniqueID("fork-signalled"), framework.Spec{
		Args: []string{"sh", "-c", script},
		Annotations: map[string]string{
			container.AnnotationForkFrom:   dir,
			container.AnnotationForkSignal: "SIGUSR2",
		},
	})
	for _, c := range []*framework.Container{quiet, signalled} {
		var info container.ForkInfo
		s, err := c.ReadFile(container.ForkFile)
		if err != nil || json.Unmarshal([]byte(s), &info) != nil {
			t.Fatalf("%s: %s is %q, %v", c.ID, container.ForkFile, s, err)
		}
		if info.ID != c.ID || info.Snapshot != dir || info.Created.IsZero() {
			t.Fatalf("%s: %s says %+v", c.ID, container.ForkFile, info)
		}
		// Owned by the container's root, as anything it made itself.
		if out, code := c.Exec("stat", "-c", "%u", container.ForkFile); code != 0 || strings.TrimSpace(out) != "0" {
			t.Fatalf("%s: %s owned by %q", c.ID, container.ForkFile, out)
		}
	}
	framework.Eventually(t, time.Minute, "the signalled fork's handler to run", func() bool {
		s, _ := signalled.ReadFile("/tmp/forked")
		return strings.Contains(s, `"id":"`+signalled.ID+`"`)
	})
	if _, err := quiet.ReadFile("/tmp/forked"); err == nil {
		t.Fatal("a fork that asked for no signal got one")
	}
	if _, err := parent.ReadFile(container.ForkFile); err == nil {
		t.Fatal("the parent has a fork file")
	}
	if st := signalled.Status(); st != containerd.Running {
		t.Fatalf("the signalled fork is %s", st)
	}
}
