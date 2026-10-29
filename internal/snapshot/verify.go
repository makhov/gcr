//go:build linux

package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/errdefs"

	"github.com/makhov/gcr/internal/userns"
)

// owner is who must own a snapshot's directories and files: root, which
// gcr runs as; a variable for tests.
var owner = 0

// Verify checks that the snapshot in dir, and the snapshot of every layer
// it names, is gcr's to fork from: under one of roots, reached without a
// symlink, and owned by root and writable by no one else, from the root
// down to its files. Whoever could write any of it could choose what a
// fork mounts and what CRIU restores, as root.
func Verify(dir string, roots []string) error {
	if err := verifyOne(dir, roots, true); err != nil {
		return err
	}
	layers, err := readLayers(dir)
	if err != nil {
		return err
	}
	for _, l := range layers {
		d, err := Owner(l)
		if err != nil {
			return err
		}
		if err := verifyOne(d, roots, false); err != nil {
			return fmt.Errorf("layer %s: %w", l, err)
		}
	}
	return nil
}

func verifyOne(dir string, roots []string, withImages bool) error {
	refuse := func(why string, args ...any) error {
		return fmt.Errorf("%s: %s: %w", dir, fmt.Sprintf(why, args...), errdefs.ErrPermissionDenied)
	}
	if !filepath.IsAbs(dir) {
		return refuse("not an absolute path")
	}
	dir = filepath.Clean(dir)
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if real != dir {
		return refuse("reached through a symlink (it is %s)", real)
	}
	var root string
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(rr, dir); err == nil && rel != "." && filepath.IsLocal(rel) {
			root = rr
			break
		}
	}
	if root == "" {
		return refuse("not under SnapshotRoots %v", roots)
	}
	// The root and everything below it on the way, then the snapshot.
	for p := dir; ; p = filepath.Dir(p) {
		if err := ownedDir(p); err != nil {
			return refuse("%v", err)
		}
		if p == root {
			break
		}
	}
	parts := []string{Upper}
	if withImages {
		parts = append(parts, "images")
	}
	for _, p := range parts {
		if err := ownedDir(filepath.Join(dir, p)); err != nil {
			return refuse("%v", err)
		}
	}
	for _, f := range []string{LayersFile, userns.KeyFile, userns.BaseFile} {
		err := ownedFile(filepath.Join(dir, f))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return refuse("%v", err)
		}
	}
	return nil
}

func ownedDir(path string) error {
	return owned(path, fs.ModeDir)
}

func ownedFile(path string) error {
	return owned(path, 0)
}

// owned checks that path is of type typ (no symlink followed), owned by
// owner and not writable by group or others.
func owned(path string, typ fs.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().Type() != typ {
		want := map[fs.FileMode]string{fs.ModeDir: "a directory", 0: "a regular file"}[typ]
		if info.Mode().Type() == fs.ModeSymlink {
			return fmt.Errorf("%s is a symlink, not %s", path, want)
		}
		return fmt.Errorf("%s is not %s", path, want)
	}
	st := info.Sys().(*syscall.Stat_t)
	if int(st.Uid) != owner {
		return fmt.Errorf("%s is owned by uid %d, not root", path, st.Uid)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by others (%v)", path, info.Mode().Perm())
	}
	return nil
}

// readLayers reads dir's LayersFile without checking the layers exist.
func readLayers(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, LayersFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var layers []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			layers = append(layers, l)
		}
	}
	return layers, nil
}
