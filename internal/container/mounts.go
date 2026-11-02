//go:build linux

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/mnt"
	"github.com/containerd/errdefs"
	devices "github.com/opencontainers/cgroups/devices/config"
	"github.com/opencontainers/runc/libcontainer/configs"
	"golang.org/x/sys/unix"
)

// checkMounts refuses CRIU images whose container had bind mounts other
// than this one's: CRIU restores a bind mount from outside the container
// (a pod's /etc/hosts, its service account's token, its volumes) as this
// container's mount at the same place, and has none for one it lacks,
// while a mount this container has and the images do not is simply not
// there for the restored processes. A fork taken elsewhere (an image made
// in CI) must have been run with its pod's mounts.
func checkMounts(dir string, cfg *configs.Config) error {
	imgs, err := filepath.Glob(filepath.Join(dir, "mountpoints-*.img"))
	if err != nil {
		return err
	}
	had := map[string]bool{}
	for _, path := range imgs {
		in, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		img, err := crit.New(in, nil, "", false, false).Decode(&mnt.MntEntry{})
		in.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		for _, e := range img.Entries {
			m, ok := e.Message.(*mnt.MntEntry)
			if !ok {
				return fmt.Errorf("%s: entry is %T", filepath.Base(path), e.Message)
			}
			// libcontainer keys a bind mount from the spec by where it
			// goes; masked paths are keyed by /dev/null.
			if k := m.GetExtKey(); k != "" && k == m.GetMountpoint() {
				had[k] = true
			}
		}
	}
	has := map[string]bool{}
	for _, m := range cfg.Mounts {
		if m.IsBind() {
			has[m.Destination] = true
		}
	}
	for _, d := range cfg.Devices {
		// Bound from the host where they cannot be created (in a user
		// namespace).
		has[d.Path] = true
	}
	var missing, extra []string
	for d := range had {
		if !has[d] {
			missing = append(missing, d)
		}
	}
	for d := range has {
		if !had[d] && !isDevice(cfg, d) {
			extra = append(extra, d)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	slices.Sort(missing)
	slices.Sort(extra)
	var why []string
	if len(missing) > 0 {
		why = append(why, "the snapshot's container had "+strings.Join(missing, ", ")+" mounted, and this one has not")
	}
	if len(extra) > 0 {
		why = append(why, "this container mounts "+strings.Join(extra, ", ")+", and the snapshot's did not")
	}
	return fmt.Errorf("%s: a snapshot restores only into a container with the same mounts: %w",
		strings.Join(why, "; "), errdefs.ErrFailedPrecondition)
}

// isDevice says whether path is one of the container's devices: created,
// or bound where they cannot be, so either way is the container's own.
func isDevice(cfg *configs.Config, path string) bool {
	return slices.ContainsFunc(cfg.Devices, func(d *devices.Device) bool { return d.Path == path })
}
