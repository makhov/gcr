//go:build linux

package container

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"
)

// ForkFile is where a fork finds out that it is one, in its own root.
// Every fork of a snapshot resumes with the same memory, so whatever a
// program generated or seeded before the snapshot (random number
// generators, IDs, tokens) is the same in each; a program that reads this
// file, or waits for AnnotationForkSignal, can tell its forks apart and
// start those over.
const ForkFile = "/.gcr/fork.json"

// AnnotationForkSignal names a signal (SIGUSR2, USR2 or 12) gcr sends a
// fork's init once it is restored: the moment to re-seed. Off by default,
// since a process that does not handle SIGUSR1 or SIGUSR2 dies of it.
const AnnotationForkSignal = "gcr.dev/fork-signal"

// ForkInfo is ForkFile's contents.
type ForkInfo struct {
	// ID is the fork's container ID, unique among the forks of a snapshot.
	ID string `json:"id"`
	// Snapshot is the directory the fork started from.
	Snapshot string `json:"snapshot"`
	// Created is when the fork was created.
	Created time.Time `json:"created"`
}

// ParseSignal reads a signal by name (SIGUSR2, USR2) or number.
func ParseSignal(s string) (unix.Signal, error) {
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 65 {
		return unix.Signal(n), nil
	}
	name := strings.ToUpper(s)
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	if sig := unix.SignalNum(name); sig != 0 {
		return sig, nil
	}
	return 0, fmt.Errorf("%s=%q: not a signal: %w", AnnotationForkSignal, s, errdefs.ErrInvalidArgument)
}

// writeForkInfo puts ForkFile into the writable layer of an overlay rootfs
// before it is mounted, owned by the container's root (root, the host
// ID it maps to). It shadows the one a snapshot of a fork brought along.
func writeForkInfo(mounts []mount.Mount, info ForkInfo, root int) error {
	upper := ""
	for _, m := range mounts {
		for _, o := range m.Options {
			if v, ok := strings.CutPrefix(o, "upperdir="); ok {
				upper = v
			}
		}
	}
	if upper == "" {
		return errRootfs("forks")
	}
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	dir := filepath.Join(upper, filepath.Dir(ForkFile))
	path := filepath.Join(upper, ForkFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	for _, p := range []string{dir, path} {
		if err := os.Lchown(p, root, root); err != nil {
			return err
		}
	}
	return nil
}
