//go:build linux

package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/errdefs"
)

// newSnapshot makes a snapshot directory under the test's, with layers
// listed as its own.
func newSnapshot(t *testing.T, name string, layers ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, Upper), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "images"), 0o700); err != nil {
		t.Fatal(err)
	}
	if len(layers) > 0 {
		b := []byte("")
		for _, l := range layers {
			b = append(b, l+"\n"...)
		}
		if err := os.WriteFile(filepath.Join(dir, LayersFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func upper(dir string) string { return filepath.Join(dir, Upper) }

func users(t *testing.T, dir string) []User {
	t.Helper()
	u, err := Users(dir)
	if err != nil {
		t.Fatalf("users of %s: %v", dir, err)
	}
	return u
}

func TestContainerHolds(t *testing.T) {
	a := newSnapshot(t, "a")
	b := newSnapshot(t, "b")
	h, err := Acquire("ns", "c1", []string{upper(b), upper(a)})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{a, b} {
		u := users(t, dir)
		if len(u) != 1 || u[0].Kind != Container || u[0].Namespace != "ns" || u[0].ID != "c1" {
			t.Fatalf("users of %s: %+v, want container ns/c1", dir, u)
		}
		if err := Remove(dir); !errdefs.IsFailedPrecondition(err) {
			t.Fatalf("remove %s in use: %v, want refused", dir, err)
		}
	}
	if _, err := Acquire("ns", "c1", []string{upper(a)}); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("second hold for the same container: %v", err)
	}
	h.Release()
	h.Release() // a container deleted twice
	for _, dir := range []string{a, b} {
		if u := users(t, dir); len(u) != 0 {
			t.Fatalf("users of %s after release: %+v", dir, u)
		}
		if err := Remove(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s still there: %v", dir, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(a))
	if len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	if _, err := Acquire("ns", "c2", []string{upper(a)}); !errdefs.IsNotFound(err) {
		t.Fatalf("hold on a removed snapshot: %v", err)
	}
}

// A container whose shim died holds no lock: its reference is cleared.
func TestStaleContainer(t *testing.T) {
	a := newSnapshot(t, "a")
	if err := os.Mkdir(filepath.Join(a, RefsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(User{Kind: Container, Namespace: "ns", ID: "dead"})
	ref := filepath.Join(a, RefsDir, "container-ns-dead")
	if err := os.WriteFile(ref, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if u := users(t, a); len(u) != 0 {
		t.Fatalf("users: %+v, want none", u)
	}
	if _, err := os.Stat(ref); !os.IsNotExist(err) {
		t.Fatalf("stale reference kept: %v", err)
	}
	// And a new container of that name can take its place.
	h, err := Acquire("ns", "dead", []string{upper(a)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if u := users(t, a); len(u) != 1 {
		t.Fatalf("users: %+v", u)
	}
}

// A snapshot of a fork of a holds a reference on a while it exists.
func TestSnapshotRefers(t *testing.T) {
	a := newSnapshot(t, "a")
	b := newSnapshot(t, "b", upper(a))
	if err := Record(b, []string{upper(a)}); err != nil {
		t.Fatal(err)
	}
	u := users(t, a)
	if len(u) != 1 || u[0].Kind != Snapshot || u[0].Path != b {
		t.Fatalf("users of a: %+v, want snapshot %s", u, b)
	}
	if err := Remove(a); !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("remove a: %v, want refused", err)
	}
	if err := Remove(b); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(a, RefsDir)); len(entries) != 0 {
		t.Fatalf("b's reference on a kept: %v", entries)
	}
	if err := Remove(a); err != nil {
		t.Fatal(err)
	}
}

// A reference from a snapshot that no longer lists the layer is stale:
// its directory was replaced, or removed by hand.
func TestStaleSnapshot(t *testing.T) {
	a := newSnapshot(t, "a")
	b := newSnapshot(t, "b", upper(a))
	if err := Record(b, []string{upper(a)}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(b); err != nil {
		t.Fatal(err)
	}
	if u := users(t, a); len(u) != 0 {
		t.Fatalf("users of a: %+v, want none", u)
	}
	if err := Remove(a); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveRefusesOtherDirectories(t *testing.T) {
	for name, setup := range map[string]func(dir string) error{
		"no upper": func(dir string) error { return os.Mkdir(filepath.Join(dir, "images"), 0o700) },
		"other files": func(dir string) error {
			if err := os.Mkdir(filepath.Join(dir, Upper), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600)
		},
	} {
		dir := t.TempDir()
		if err := setup(dir); err != nil {
			t.Fatal(err)
		}
		if err := Remove(dir); !errdefs.IsInvalidArgument(err) {
			t.Fatalf("%s: %v, want refused", name, err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s: removed anyway", name)
		}
	}
	if err := Remove(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}
}

func TestOwner(t *testing.T) {
	if d, err := Owner("/s/a/upper"); err != nil || d != "/s/a" {
		t.Fatalf("Owner: %q, %v", d, err)
	}
	if _, err := Owner("/var/lib/layer"); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("Owner of a layer not a snapshot's: %v", err)
	}
}
