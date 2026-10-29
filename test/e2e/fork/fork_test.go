//go:build e2e && linux

// Package fork checks forking: many containers started from one
// snapshot, each carrying on from the state it holds. The snapshot's
// writable layer stays underneath every fork's own, so a fork costs an
// empty layer and no copy, and what one writes no other one sees.
package fork

import (
	"fmt"
	"os"
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
}
