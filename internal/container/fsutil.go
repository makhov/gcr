//go:build linux

package container

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// copyTree copies the contents of src into dst (created if missing). cp -a
// keeps overlay whiteouts (0:0 char devices); --reflink=auto shares data
// blocks where the filesystem supports it. The xattrs (trusted.overlay.*
// making a directory opaque, file capabilities) are copied here: BusyBox's
// cp, the one k0s nodes have, drops them, and GNU cp ignores failing to
// keep them.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	out, err := exec.Command("cp", "-a", "--reflink=auto", src+"/.", dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp %s: %w: %s", src, err, out)
	}
	return filepath.WalkDir(src, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		xattrs, err := Xattrs(p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, strings.TrimPrefix(p, src))
		for name, v := range xattrs {
			if err := unix.Lsetxattr(to, name, []byte(v), 0); err != nil {
				return fmt.Errorf("copy %s: %s: %w", p, name, err)
			}
		}
		return nil
	})
}

// Xattrs are p's extended attributes (not a symlink's target's): none
// where the filesystem has none.
func Xattrs(p string) (map[string]string, error) {
	size, err := unix.Llistxattr(p, nil)
	if err != nil || size == 0 {
		if errors.Is(err, unix.ENOTSUP) {
			err = nil
		}
		return nil, err
	}
	buf := make([]byte, size)
	if size, err = unix.Llistxattr(p, buf); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:size]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		n, err := unix.Lgetxattr(p, name, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", p, name, err)
		}
		v := make([]byte, n)
		if n, err = unix.Lgetxattr(p, name, v); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", p, name, err)
		}
		out[name] = string(v[:n])
	}
	return out, nil
}

// emptyDir removes everything inside dir, keeping dir itself.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
