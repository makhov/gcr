//go:build linux

package snapimage

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"golang.org/x/sys/unix"
)

func TestAllowed(t *testing.T) {
	for _, tc := range []struct {
		image    string
		patterns []string
		want     bool
	}{
		{"reg.example.com/snaps/app:v1", nil, false},
		{"reg.example.com/snaps/app:v1", []string{"*"}, true},
		{"reg.example.com/snaps/app:v1", []string{"reg.example.com/snaps/*"}, true},
		{"reg.example.com/other/app:v1", []string{"reg.example.com/snaps/*"}, false},
		{"reg.example.com/snaps/app:v1", []string{"reg.example.com/snaps/app"}, true},
		{"reg.example.com/snaps/app@sha256:ab", []string{"reg.example.com/snaps/app"}, true},
		{"reg.example.com:5000/snaps/app", []string{"reg.example.com:5000/snaps/app"}, true},
		{"reg.example.com/snaps/app2:v1", []string{"reg.example.com/snaps/app"}, false},
		{"", []string{"reg.example.com/*"}, false},
		{"", []string{"*"}, true},
	} {
		if got := Allowed(tc.image, tc.patterns); got != tc.want {
			t.Errorf("Allowed(%q, %q) = %v, want %v", tc.image, tc.patterns, got, tc.want)
		}
	}
}

func TestFind(t *testing.T) {
	top, mid, low := t.TempDir(), t.TempDir(), t.TempDir()
	for _, d := range []string{mid, low} {
		if err := os.MkdirAll(filepath.Join(d, Dir, "images"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, Dir, InfoFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	overlay := func(lowers ...string) []mount.Mount {
		return []mount.Mount{{Type: "overlay", Source: "overlay", Options: []string{
			"index=off", "workdir=/w", "upperdir=/u", "lowerdir=" + filepath.Join(lowers[0]) + func() string {
				s := ""
				for _, l := range lowers[1:] {
					s += ":" + l
				}
				return s
			}()}}}
	}
	if got := Find(overlay(top, mid, low)); got != filepath.Join(mid, Dir) {
		t.Errorf("Find = %q, want the topmost layer's, %q", got, filepath.Join(mid, Dir))
	}
	if got := Find(overlay(top)); got != "" {
		t.Errorf("Find in an image without one = %q", got)
	}
	if got := Find([]mount.Mount{{Type: "bind", Source: low}}); got != filepath.Join(low, Dir) {
		t.Errorf("Find in a bind mount = %q", got)
	}
	// A layer above with only the directory (no info) hides those below.
	if err := os.Mkdir(filepath.Join(top, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := Find(overlay(top, mid)); got != "" {
		t.Errorf("Find under a layer that hides it = %q", got)
	}
}

func TestWriteUpper(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("whiteouts are character devices: needs root")
	}
	dir := t.TempDir()
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "etc", "opaque"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("etc/conf", "hello")
	if err := os.Link(filepath.Join(dir, "etc/conf"), filepath.Join(dir, "etc/conf2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("conf", filepath.Join(dir, "etc/link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mknod(filepath.Join(dir, "etc/gone"), unix.S_IFCHR, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(dir, "etc/opaque"), "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		t.Skipf("trusted xattrs here: %v", err)
	}
	if err := unix.Setxattr(filepath.Join(dir, "etc/conf"), "user.keep", []byte("v"), 0); err != nil {
		t.Skipf("user xattrs here: %v", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeUpper(tw, dir); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	got := map[string]*tar.Header{}
	var names []string
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = h
		names = append(names, h.Name)
	}
	want := []string{"etc/", "etc/conf", "etc/conf2", "etc/.wh.gone", "etc/link", "etc/opaque/", "etc/opaque/.wh..wh..opq"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("entries %q, want %q", names, want)
	}
	if h := got["etc/conf2"]; h.Typeflag != tar.TypeLink || h.Linkname != "etc/conf" {
		t.Errorf("hard link: %+v", h)
	}
	if h := got["etc/conf"]; h.PAXRecords["SCHILY.xattr.user.keep"] != "v" {
		t.Errorf("xattr not kept: %v", h.PAXRecords)
	}
	for _, n := range names {
		for k := range got[n].PAXRecords {
			if slices.Contains([]string{"SCHILY.xattr.trusted.overlay.opaque"}, k) {
				t.Errorf("%s keeps overlay's %s", n, k)
			}
		}
	}

	// Deterministic: the same directory, the same bytes.
	var again bytes.Buffer
	tw = tar.NewWriter(&again)
	if err := writeUpper(tw, dir); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	got2 := again.Bytes()
	var first bytes.Buffer
	tw = tar.NewWriter(&first)
	_ = writeUpper(tw, dir)
	_ = tw.Close()
	if !bytes.Equal(first.Bytes(), got2) {
		t.Error("two packs of one directory differ")
	}
}

func TestWriteState(t *testing.T) {
	images := t.TempDir()
	if err := os.WriteFile(filepath.Join(images, "core-1.img"), []byte("core"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeState(tw, images, Info{Arch: "arm64", Created: time.Unix(0, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	var names []string
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	want := []string{Dir + "/", Dir + "/.wh..wh..opq", Dir + "/" + InfoFile, Dir + "/images/", Dir + "/images/core-1.img"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("entries %q, want %q", names, want)
	}
}
