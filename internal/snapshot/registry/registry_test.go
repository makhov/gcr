//go:build linux

package registry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"oras.land/oras-go/v2/content/memory"

	"github.com/makhov/gcr/internal/snapshot"
)

// newSnapshot makes a snapshot directory with files in its upper (path
// -> content) and layers beneath it.
func newSnapshot(t *testing.T, dir string, files map[string]string, layers ...string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, snapshot.Upper, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "pages-1.img"), []byte(strings.Repeat("page", 1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(layers) > 0 {
		if err := os.WriteFile(filepath.Join(dir, snapshot.LayersFile), []byte(strings.Join(layers, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPushPull(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	a := filepath.Join(src, "a")
	b := filepath.Join(src, "b")
	newSnapshot(t, a, map[string]string{"parent": "parent", "etc/conf": "1"})
	newSnapshot(t, b, map[string]string{"fork": "b", "etc/conf": "2"}, filepath.Join(a, snapshot.Upper))

	reg := memory.New()
	desc, err := Push(ctx, b, reg, "b")
	if err != nil {
		t.Fatal(err)
	}
	// Packing is deterministic: a second push, without the cache, makes
	// the same blobs, and so the same manifest bar its creation time.
	for _, d := range []string{a, b} {
		if err := os.Remove(filepath.Join(d, BlobsFile)); err != nil {
			t.Fatal(err)
		}
	}
	again, err := Push(ctx, b, memory.New(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if desc.Size != again.Size {
		t.Fatalf("pushing twice: manifests of %d and %d bytes", desc.Size, again.Size)
	}

	dst := t.TempDir()
	store := filepath.Join(dst, "layers")
	c := filepath.Join(dst, "c")
	if err := Pull(ctx, reg, "b", c, store); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(c, snapshot.Upper, "fork")); got != "b" {
		t.Fatalf("pulled snapshot's /fork: %q", got)
	}
	if got := readFile(t, filepath.Join(c, "images", "pages-1.img")); len(got) != 4096 {
		t.Fatalf("pulled images: %d bytes", len(got))
	}
	// Its layer beneath is in the store, and its LayersFile names it.
	layers := strings.Fields(readFile(t, filepath.Join(c, snapshot.LayersFile)))
	if len(layers) != 1 || !strings.HasPrefix(layers[0], store+"/") {
		t.Fatalf("pulled layers: %v, want one in %s", layers, store)
	}
	if got := readFile(t, filepath.Join(layers[0], "parent")); got != "parent" {
		t.Fatalf("layer beneath: /parent is %q", got)
	}
	layerDir := filepath.Dir(layers[0])
	users, err := snapshot.Users(layerDir)
	if err != nil || len(users) != 1 || users[0].Path != c {
		t.Fatalf("users of the pulled layer: %v, %v; want snapshot %s", users, err, c)
	}

	// A second snapshot with the same layer beneath shares it.
	d := filepath.Join(dst, "d")
	if err := Pull(ctx, reg, "b", d, store); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(store); len(entries) != 1 {
		t.Fatalf("store has %d layers after two pulls of one, want 1", len(entries))
	}
	if users, _ := snapshot.Users(layerDir); len(users) != 2 {
		t.Fatalf("users of the shared layer: %v", users)
	}

	// Removing works as for a snapshot taken here: the layer only once
	// nothing mounts it.
	if err := snapshot.Remove(layerDir); !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("remove a layer in use: %v", err)
	}
	for _, s := range []string{c, d, layerDir} {
		if err := snapshot.Remove(s); err != nil {
			t.Fatalf("remove %s: %v", s, err)
		}
	}

	if err := Pull(ctx, reg, "b", src, store); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("pull into a directory with files: %v", err)
	}
	if err := Pull(ctx, reg, "missing", filepath.Join(dst, "e"), store); err == nil {
		t.Fatal("pulled a tag that is not there")
	}
}

// A snapshot a registry has the blobs of already is pushed by reference,
// from its cached descriptors.
func TestPushCached(t *testing.T) {
	ctx := context.Background()
	a := filepath.Join(t.TempDir(), "a")
	newSnapshot(t, a, map[string]string{"x": "1"})
	reg := memory.New()
	if _, err := Push(ctx, a, reg, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a, BlobsFile)); err != nil {
		t.Fatalf("no cache: %v", err)
	}
	if _, err := Push(ctx, a, reg, "two"); err != nil {
		t.Fatal(err)
	}
	// Another registry has none of them: they are packed again, and must
	// come out the same.
	if _, err := Push(ctx, a, memory.New(), "one"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, snapshot.Upper, "x"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, a, memory.New(), "one"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("pushing a snapshot changed after it was packed: %v", err)
	}
}
