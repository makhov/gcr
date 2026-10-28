//go:build linux

package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"
)

// LayersFile lists, in a snapshot or a checkpoint, the read-only layers
// the container had beneath its writable layer, one path a line, top
// first: the writable layers of the snapshots it was forked from.
const LayersFile = "gcr-layers"

// ForkLayers returns the read-only layers a fork of the snapshot in dir
// mounts beneath its own writable layer: the snapshot's writable layer,
// then the ones the snapshot's container had (a fork of a fork). A fork
// shares them with the snapshot and every other fork of it, so it costs
// an empty layer, whatever the snapshot's size.
func ForkLayers(dir string) ([]string, error) {
	below, err := ReadLayers(dir)
	if err != nil {
		return nil, err
	}
	upper := filepath.Join(dir, "upper")
	if err := checkLayer(upper); err != nil {
		return nil, err
	}
	return append([]string{upper}, below...), nil
}

// ReadLayers reads the LayersFile in dir; without one there are none.
func ReadLayers(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, LayersFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var layers []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if err := checkLayer(l); err != nil {
			return nil, err
		}
		layers = append(layers, l)
	}
	return layers, nil
}

// checkLayer says whether path can be an overlay lower layer: absolute,
// without the characters that separate overlay options, and still there
// (a snapshot has to outlive the forks of it and of its forks).
func checkLayer(path string) error {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, ":,") {
		return fmt.Errorf("layer %q: must be an absolute path without ':' or ',': %w", path, errdefs.ErrInvalidArgument)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("layer %q: %w", path, err)
	}
	return nil
}

func writeLayers(dir string, layers []string) error {
	if len(layers) == 0 {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, LayersFile), []byte(strings.Join(layers, "\n")+"\n"), 0o600)
}

// mountRootfs mounts the rootfs with layers, read-only and top first,
// between the overlay's writable layer and the image's layers.
//
// containerd's mount code cannot do this for a container in a user
// namespace: it idmaps every lower layer, and these hold host IDs already
// (they were a container's writable layer, in the range this one maps
// too), and it needs all lower layers under one directory. So gcr idmaps
// the image's layers itself, each on its own, and leaves the others be.
func mountRootfs(mounts []mount.Mount, target string, layers []string) (retErr error) {
	if len(layers) == 0 {
		return mountAll(mounts, target)
	}
	if len(mounts) != 1 || mounts[0].Type != "overlay" {
		return fmt.Errorf("fork needs an overlay rootfs: %w", errdefs.ErrNotImplemented)
	}
	var opts, lowers []string
	var idmap string
	for _, o := range mounts[0].Options {
		if v, ok := strings.CutPrefix(o, "lowerdir="); ok {
			lowers = strings.Split(v, ":")
		} else if v, ok := strings.CutPrefix(o, "uidmap="); ok {
			idmap = v // gidmap is the same (idmapRootfs)
		} else if !strings.HasPrefix(o, "gidmap=") {
			opts = append(opts, o)
		}
	}
	if len(lowers) == 0 {
		return fmt.Errorf("fork needs an overlay rootfs with image layers: %w", errdefs.ErrNotImplemented)
	}
	if idmap != "" {
		mapped, cleanup, err := idmapLayers(lowers, idmap, filepath.Dir(target))
		if err != nil {
			return err
		}
		// The overlay holds on to the layers it was given; their mounts
		// are not needed once it is up.
		defer cleanup()
		lowers = mapped
	}
	opts = append(opts, "lowerdir="+strings.Join(slices.Concat(layers, lowers), ":"))
	return mount.All([]mount.Mount{{Type: "overlay", Source: mounts[0].Source, Options: opts}}, target)
}

// idmapLayers mounts each of layers, read-only and idmapped with idmap,
// under a new directory in dir, and returns their paths and a function
// that unmounts and removes them.
func idmapLayers(layers []string, idmap, dir string) (_ []string, _ func(), retErr error) {
	userns, err := mount.GetUsernsFD(idmap, idmap)
	if err != nil {
		return nil, nil, fmt.Errorf("idmap image layers: %w", err)
	}
	defer userns.Close()
	tmp, err := os.MkdirTemp(dir, "layers-")
	if err != nil {
		return nil, nil, err
	}
	var mapped []string
	cleanup := func() {
		for _, p := range mapped {
			_ = unix.Unmount(p, unix.MNT_DETACH)
			_ = os.Remove(p)
		}
		_ = os.Remove(tmp) // not RemoveAll: never into a layer still mounted
	}
	defer func() {
		if retErr != nil {
			cleanup()
		}
	}()
	for i, l := range layers {
		p := filepath.Join(tmp, strconv.Itoa(i))
		if err := os.Mkdir(p, 0o700); err != nil {
			return nil, nil, err
		}
		if err := mount.IDMapMountWithAttrs(l, p, int(userns.Fd()), unix.MOUNT_ATTR_RDONLY, 0); err != nil {
			_ = os.Remove(p)
			return nil, nil, fmt.Errorf("idmap image layer: %w", err)
		}
		mapped = append(mapped, p)
	}
	return mapped, cleanup, nil
}
