//go:build linux

package container

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// copyTree keeps xattrs whatever cp does with them.
func TestCopyTreeXattrs(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	dir := filepath.Join(src, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(dir, "user.gcr-test", []byte("y"), 0); errors.Is(err, unix.ENOTSUP) {
		t.Skip("no user xattrs here")
	} else if err != nil {
		t.Fatal(err)
	}
	// A cp that drops xattrs, as BusyBox's does.
	cp, err := exec.LookPath("cp")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "cp"), []byte("#!/bin/sh\nshift 2\nexec "+cp+" -dR \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := Xattrs(filepath.Join(dst, "d"))
	if err != nil || got["user.gcr-test"] != "y" {
		t.Fatalf("xattrs %v, %v", got, err)
	}
}
