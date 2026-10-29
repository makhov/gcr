//go:build e2e && linux

package fork

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/errdefs"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/snapshot"
	"github.com/makhov/gcr/test/e2e/framework"
)

// TestSnapshotLifecycle checks that a snapshot stays while anything
// mounts its layer: the forks of it, and the snapshots of those, down to
// a fork of a fork. Each goes away in turn, and only then does it.
func TestSnapshotLifecycle(t *testing.T) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	spec := framework.Spec{Args: []string{"sh", "-c", counter}}
	parent := ns.Run(framework.UniqueID("parent"), spec)
	first := filepath.Join(framework.TempDir(t), "first")
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), first); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Writing over a snapshot would change its forks' files under them.
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), first); err == nil {
		t.Fatal("a second snapshot into the same directory succeeded")
	}
	parent.Remove() // a snapshot does not need its container

	fork := func(name, dir string) *framework.Container {
		s := spec
		s.Annotations = map[string]string{container.AnnotationForkFrom: dir}
		return ns.Run(framework.UniqueID(name), s)
	}
	a := fork("fork-a", first)
	second := filepath.Join(framework.TempDir(t), "second")
	if err := control.NewClient(ns.Name, a.ID).Snapshot(t.Context(), second); err != nil {
		t.Fatalf("snapshot of a fork: %v", err)
	}
	b := fork("fork-b", second)

	users := func(dir string) map[string]bool {
		t.Helper()
		us, err := snapshot.Users(dir)
		if err != nil {
			t.Fatalf("users of %s: %v", dir, err)
		}
		m := map[string]bool{}
		for _, u := range us {
			m[u.String()] = true
		}
		return m
	}
	inUse := func(dir string, want ...string) {
		t.Helper()
		got := users(dir)
		for _, w := range want {
			if !got[w] {
				t.Fatalf("users of %s: %v, want %q among them", filepath.Base(dir), got, w)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("users of %s: %v, want %v", filepath.Base(dir), got, want)
		}
		if err := snapshot.Remove(dir); !errdefs.IsFailedPrecondition(err) {
			t.Fatalf("remove %s in use: %v", filepath.Base(dir), err)
		}
	}
	ctr := func(c *framework.Container) string { return "container " + ns.Name + "/" + c.ID }

	// b mounts both snapshots' layers, and second lists first's.
	inUse(first, ctr(a), ctr(b), "snapshot "+second)
	inUse(second, ctr(b))

	a.Remove()
	inUse(first, ctr(b), "snapshot "+second)
	b.Remove()
	inUse(first, "snapshot "+second)
	if len(users(second)) != 0 {
		t.Fatalf("second still in use: %v", users(second))
	}
	if err := snapshot.Remove(second); err != nil {
		t.Fatalf("remove second: %v", err)
	}
	if err := snapshot.Remove(first); err != nil {
		t.Fatalf("remove first: %v", err)
	}
	for _, dir := range []string{first, second} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", dir, err)
		}
	}

	// A fork of a removed snapshot fails to start, rather than run with
	// files missing.
	c := ns.Create(framework.UniqueID("fork-gone"), framework.Spec{
		Args:        spec.Args,
		Annotations: map[string]string{container.AnnotationForkFrom: first},
	})
	if err := c.Start(); err == nil {
		t.Fatal("a fork of a removed snapshot started")
	}
}
