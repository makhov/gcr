//go:build e2e && linux

// Package fork checks forking: many containers started from one
// snapshot, each carrying on from the state it holds. The snapshot's
// writable layer stays underneath every fork's own, so a fork costs an
// empty layer and no copy, and what one writes no other one sees.
package fork

import (
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"

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

func TestFork(t *testing.T) {
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
	for i := range children {
		children[i] = ns.Create(framework.UniqueID(fmt.Sprintf("fork%d", i)), framework.Spec{
			Args:        []string{"sh", "-c", counter},
			Annotations: map[string]string{container.AnnotationForkFrom: dir},
		})
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = children[i].Start() }(i)
	}
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
	t.Logf("%d forks running from one snapshot", forks)

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
