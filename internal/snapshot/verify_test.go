//go:build linux

package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/errdefs"

	"github.com/makhov/gcr/internal/userns"
)

func TestVerify(t *testing.T) {
	owner = os.Getuid()
	t.Cleanup(func() { owner = 0 })
	root := filepath.Join(t.TempDir(), "snapshots")
	mk := func(name string, layers ...string) string {
		t.Helper()
		dir := filepath.Join(root, name)
		for _, d := range []string{Upper, "images"} {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if len(layers) > 0 {
			if err := os.WriteFile(filepath.Join(dir, LayersFile), []byte(strings.Join(layers, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	roots := []string{root}
	a := mk("a")
	b := mk("b", filepath.Join(a, Upper))
	if err := Verify(b, roots); err != nil {
		t.Fatalf("a snapshot and its layer under the root: %v", err)
	}

	refused := func(what, dir string) {
		t.Helper()
		if err := Verify(dir, roots); !errdefs.IsPermissionDenied(err) {
			t.Errorf("%s: %v, want refused", what, err)
		}
	}
	// Outside the roots, or the root itself.
	outside := filepath.Join(t.TempDir(), "x")
	for _, d := range []string{Upper, "images"} {
		_ = os.MkdirAll(filepath.Join(outside, d), 0o755)
	}
	refused("outside the roots", outside)
	refused("the root itself", root)

	// Reached through a symlink.
	link := filepath.Join(root, "link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	refused("a symlink to a snapshot", link)

	// A symlinked part: upper pointing anywhere (host root, say).
	c := mk("c")
	_ = os.RemoveAll(filepath.Join(c, Upper))
	if err := os.Symlink("/", filepath.Join(c, Upper)); err != nil {
		t.Fatal(err)
	}
	refused("upper a symlink to /", c)

	// A key file that is a symlink.
	d := mk("d")
	if err := os.Symlink("/etc/hostname", filepath.Join(d, userns.KeyFile)); err != nil {
		t.Fatal(err)
	}
	refused("a symlinked key file", d)

	// Writable by others.
	e := mk("e")
	if err := os.Chmod(filepath.Join(e, Upper), 0o777); err != nil {
		t.Fatal(err)
	}
	refused("a world-writable upper", e)

	// A layer outside the roots.
	f := mk("f", filepath.Join(outside, Upper))
	if err := Verify(f, roots); !errdefs.IsPermissionDenied(err) {
		t.Errorf("a layer outside the roots: %v", err)
	}

	// Another owner.
	owner = os.Getuid() + 1
	if err := Verify(a, roots); !errdefs.IsPermissionDenied(err) {
		t.Errorf("owned by someone else: %v", err)
	}
}
