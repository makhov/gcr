package imagetar

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/errdefs"
)

type entry struct {
	name, link string
	typ        byte
	body       string
}

func archive(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	err := Extract(archive(t,
		entry{name: "./", typ: tar.TypeDir},
		entry{name: "./core-1.img", typ: tar.TypeReg, body: "core"},
		entry{name: "./sub/", typ: tar.TypeDir},
		entry{name: "./sub/x.img", typ: tar.TypeReg, body: "x"},
		entry{name: "./parent", link: "../2", typ: tar.TypeSymlink},
	), dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "core-1.img")); err != nil || string(b) != "core" {
		t.Fatalf("core-1.img: %q, %v", b, err)
	}
	if l, err := os.Readlink(filepath.Join(dir, "parent")); err != nil || l != "../2" {
		t.Fatalf("parent: %q, %v", l, err)
	}
	// The same parent link again (the target's dump made it) is fine.
	if err := Extract(archive(t, entry{name: "parent", link: "../2", typ: tar.TypeSymlink}), dir); err != nil {
		t.Fatalf("the same parent link again: %v", err)
	}
}

func TestExtractRefuses(t *testing.T) {
	outside := t.TempDir()
	for name, entries := range map[string][]entry{
		"a link to the host":     {{name: "pages-1.img", link: "/etc/shadow", typ: tar.TypeSymlink}},
		"a parent link anywhere": {{name: "parent", link: "/var/lib/gcr/x/images", typ: tar.TypeSymlink}},
		"a parent climbing":      {{name: "parent", link: "../../x", typ: tar.TypeSymlink}},
		"a hard link":            {{name: "x", link: "/etc/shadow", typ: tar.TypeLink}},
		"a device":               {{name: "x", typ: tar.TypeChar}},
		"a name climbing out":    {{name: "../x", typ: tar.TypeReg, body: "x"}},
		"an absolute name":       {{name: filepath.Join(outside, "x"), typ: tar.TypeReg, body: "x"}},
	} {
		if err := Extract(archive(t, entries...), t.TempDir()); !errdefs.IsPermissionDenied(err) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}

	// A second archive cannot replace a file, nor write through a link
	// that is there (made here by hand, as no archive can).
	dir := t.TempDir()
	if err := Extract(archive(t, entry{name: "inventory.img", typ: tar.TypeReg, body: "1"}), dir); err != nil {
		t.Fatal(err)
	}
	if err := Extract(archive(t, entry{name: "inventory.img", typ: tar.TypeReg, body: "2"}), dir); err == nil {
		t.Error("a second archive replaced a file")
	}
	if err := os.Symlink(outside, filepath.Join(dir, "d")); err != nil {
		t.Fatal(err)
	}
	if err := Extract(archive(t, entry{name: "d/x", typ: tar.TypeReg, body: "x"}), dir); err == nil {
		t.Error("wrote through a link")
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
		t.Fatalf("a file appeared outside: %v", err)
	}
}
