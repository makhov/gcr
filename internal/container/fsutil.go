//go:build linux

package container

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// copyTree copies the contents of src into dst (created if missing). cp -a
// keeps overlay whiteouts (0:0 char devices) and trusted.overlay.* xattrs;
// --reflink=auto shares data blocks where the filesystem supports it.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	out, err := exec.Command("cp", "-a", "--reflink=auto", src+"/.", dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp %s: %w: %s", src, err, out)
	}
	return nil
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
