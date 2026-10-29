//go:build linux

// Package snapshot keeps track of who uses a snapshot, so that one is not
// removed while anything still mounts it.
//
// A fork mounts its snapshot's writable layer, and those of the snapshots
// that one came from, beneath its own. Removing any of them under a
// running fork changes its files under it, so each user records itself
// in the snapshot's RefsDir:
//
//   - A container holds a reference for as long as it lives, with a lock
//     on the file: if its shim dies, the lock goes with it, and the
//     reference is stale.
//   - A snapshot of a fork holds one on the snapshots its layers are
//     from, for as long as it exists and lists them.
//
// A snapshot also holds its container's user namespace range, which its
// files carry host IDs from, until it is removed: no other container
// gets those IDs while a fork may still map them.
//
// Remove refuses a snapshot with live references, and stale ones are
// cleared whenever they are read.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/userns"
)

const (
	// LayersFile lists, in a snapshot or a checkpoint, the read-only
	// layers the container had beneath its writable layer, one path a
	// line, top first: the writable layers of the snapshots it was
	// forked from.
	LayersFile = "gcr-layers"
	// RefsDir holds a file for each user of the snapshot.
	RefsDir = "gcr-refs"
	// LockFile serialises taking references with removal.
	LockFile = "gcr-lock"
	// Upper is the snapshot's copy of the container's writable layer.
	Upper = "upper"
	// BlobsFile caches a snapshot's registry blobs (package registry).
	BlobsFile = "gcr-blobs.json"
)

// known are the entries a snapshot directory has; Remove refuses a
// directory with anything else in it, which is likely not a snapshot.
var known = []string{Upper, "images", "work", LayersFile, RefsDir, LockFile, BlobsFile, userns.BaseFile, userns.KeyFile}

// Kind is what holds a reference.
type Kind string

const (
	Container Kind = "container"
	Snapshot  Kind = "snapshot"
)

// User is a live reference to a snapshot.
type User struct {
	Kind Kind `json:"kind"`
	// Namespace and ID name a container.
	Namespace string `json:"namespace,omitempty"`
	ID        string `json:"id,omitempty"`
	// Path is a snapshot's directory.
	Path  string    `json:"path,omitempty"`
	Since time.Time `json:"since"`
}

func (u User) String() string {
	if u.Kind == Container {
		return fmt.Sprintf("container %s/%s", u.Namespace, u.ID)
	}
	return "snapshot " + u.Path
}

// Owner returns the snapshot directory a layer belongs to.
func Owner(layer string) (string, error) {
	if filepath.Base(layer) != Upper {
		return "", fmt.Errorf("layer %q is not a snapshot's %s: %w", layer, Upper, errdefs.ErrInvalidArgument)
	}
	return filepath.Dir(layer), nil
}

// Hold is a running container's references on the snapshots its layers
// are from.
type Hold struct {
	refs []*os.File
}

// Acquire takes a reference for the container namespace/id on the
// snapshot of each of layers. It fails if one is gone or being removed.
func Acquire(namespace, id string, layers []string) (_ *Hold, retErr error) {
	h := &Hold{}
	defer func() {
		if retErr != nil {
			h.Release()
		}
	}()
	u := User{Kind: Container, Namespace: namespace, ID: id, Since: time.Now().UTC()}
	for _, l := range layers {
		dir, err := Owner(l)
		if err != nil {
			return nil, err
		}
		var f *os.File
		err = locked(dir, unix.LOCK_SH, func() error {
			f, err = createRef(dir, "container-"+namespace+"-"+id, u)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", dir, err)
		}
		h.refs = append(h.refs, f)
	}
	return h, nil
}

// Release drops the references. It is safe on a nil Hold.
func (h *Hold) Release() {
	if h == nil {
		return
	}
	for _, f := range h.refs {
		// Unlinked while still locked, so nothing reads it as stale and
		// removes a reference made in its place.
		_ = os.Remove(f.Name())
		_ = f.Close()
	}
	h.refs = nil
}

// createRef creates the reference file name in dir's RefsDir, holding a
// lock on it, and returns it open: the lock lasts as long as the file is.
func createRef(dir, name string, u User) (*os.File, error) {
	refs := filepath.Join(dir, RefsDir)
	if err := os.Mkdir(refs, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	path := filepath.Join(refs, name)
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, unix.EWOULDBLOCK) {
				return nil, fmt.Errorf("%s is already a user: %w", u, errdefs.ErrAlreadyExists)
			}
			return nil, err
		}
		// A reader may have removed it as stale between the open and
		// the lock: then the lock is on a file nobody sees.
		if same, err := samePath(f, path); err != nil || !same {
			f.Close()
			if err != nil {
				return nil, err
			}
			continue
		}
		b, _ := json.Marshal(u)
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := f.WriteAt(b, 0); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
}

// Record registers the snapshot just taken in from, whose container had
// layers: it holds a reference on each one's snapshot while it exists and
// lists them, and holds its container's user namespace range, which its
// files and every fork of it use, until it is removed.
func Record(from string, layers []string) error {
	from, err := filepath.Abs(from)
	if err != nil {
		return err
	}
	if r, ok, err := userRange(from); err != nil {
		return err
	} else if ok {
		if err := userns.HoldForSnapshot(from, r); err != nil {
			return err
		}
	}
	u := User{Kind: Snapshot, Path: from, Since: time.Now().UTC()}
	for _, l := range layers {
		dir, err := Owner(l)
		if err != nil {
			return err
		}
		err = locked(dir, unix.LOCK_SH, func() error {
			f, err := createRef(dir, snapshotRef(from), u)
			if err != nil {
				return err
			}
			return f.Close() // the reference is the file, not the lock
		})
		if err != nil {
			return fmt.Errorf("snapshot %s: %w", dir, err)
		}
	}
	return nil
}

func snapshotRef(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "snapshot-" + hex.EncodeToString(sum[:8])
}

// Users returns the live references to the snapshot in dir, clearing
// stale ones.
func Users(dir string) ([]User, error) {
	var users []User
	err := locked(dir, unix.LOCK_SH, func() (err error) {
		users, err = liveUsers(dir)
		return err
	})
	return users, err
}

func liveUsers(dir string) ([]User, error) {
	refs := filepath.Join(dir, RefsDir)
	entries, err := os.ReadDir(refs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var users []User
	for _, e := range entries {
		u, live, err := check(dir, filepath.Join(refs, e.Name()))
		if err != nil {
			return nil, err
		}
		if live {
			users = append(users, u)
		}
	}
	slices.SortFunc(users, func(a, b User) int { return a.Since.Compare(b.Since) })
	return users, nil
}

// check reads the reference at path and says whether it is live,
// removing it if not.
func check(dir, path string) (User, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return User{}, false, nil // released meanwhile
	}
	if err != nil {
		return User{}, false, err
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		// A container holds it, so it is alive; it may still be
		// writing the file, but it says what it is in the name.
		u := User{Kind: Container}
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &u)
		}
		if u.ID == "" {
			u.ID = strings.TrimPrefix(filepath.Base(path), "container-")
		}
		return u, true, nil
	}
	if err != nil {
		return User{}, false, err
	}
	var u User
	b, err := os.ReadFile(path)
	if err != nil {
		return User{}, false, err
	}
	live := json.Unmarshal(b, &u) == nil && u.Kind == Snapshot && lists(u.Path, filepath.Join(dir, Upper))
	if !live {
		// Unlocked and not a snapshot that still mounts dir: a container
		// that has gone, or a snapshot removed or replaced.
		if same, err := samePath(f, path); err == nil && same {
			_ = os.Remove(path)
		}
	}
	return u, live, nil
}

// lists says whether the snapshot in dir lists layer among its layers.
func lists(dir, layer string) bool {
	b, err := os.ReadFile(filepath.Join(dir, LayersFile))
	if err != nil {
		return false
	}
	return slices.Contains(strings.Split(string(b), "\n"), layer)
}

// Remove deletes the snapshot in dir, unless something uses it.
func Remove(dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := looksLikeSnapshot(dir); err != nil {
		return err
	}
	// Moved aside under the lock, so a container taking a reference
	// either did so before (and is a user) or finds the snapshot gone.
	var trash string
	err = locked(dir, unix.LOCK_EX, func() error {
		users, err := liveUsers(dir)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			names := make([]string, len(users))
			for i, u := range users {
				names[i] = u.String()
			}
			return fmt.Errorf("snapshot %s is in use by %s: %w", dir, strings.Join(names, ", "), errdefs.ErrFailedPrecondition)
		}
		if trash, err = os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".gcr-removing-"); err != nil {
			return err
		}
		return os.Rename(dir, filepath.Join(trash, "snapshot"))
	})
	if err != nil {
		return err
	}
	// The references it held on its own layers' snapshots are stale now;
	// clear them rather than wait for a reader to.
	if b, err := os.ReadFile(filepath.Join(trash, "snapshot", LayersFile)); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if owner, err := Owner(l); err == nil {
				_ = os.Remove(filepath.Join(owner, RefsDir, snapshotRef(dir)))
			}
		}
	}
	if r, ok, err := userRange(filepath.Join(trash, "snapshot")); err == nil && ok {
		if err := userns.ReleaseForSnapshot(dir, r); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.RemoveAll(trash)
}

// userRange reads the user namespace range the snapshot in dir records.
func userRange(dir string) (userns.Range, bool, error) {
	key, err := os.ReadFile(filepath.Join(dir, userns.KeyFile))
	if errors.Is(err, os.ErrNotExist) {
		return userns.Range{}, false, nil
	}
	if err != nil {
		return userns.Range{}, false, err
	}
	b, err := os.ReadFile(filepath.Join(dir, userns.BaseFile))
	if err != nil {
		return userns.Range{}, false, err
	}
	base, err := userns.ParseBase(string(b))
	if err != nil {
		return userns.Range{}, false, err
	}
	return userns.Range{Base: base, Key: strings.TrimSpace(string(key))}, true, nil
}

// looksLikeSnapshot refuses a directory that is not one: Remove deletes
// it whole.
func looksLikeSnapshot(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var upper bool
	for _, e := range entries {
		if !slices.Contains(known, e.Name()) {
			return fmt.Errorf("%s does not look like a snapshot: it has %q: %w", dir, e.Name(), errdefs.ErrInvalidArgument)
		}
		upper = upper || e.Name() == Upper
	}
	if !upper {
		return fmt.Errorf("%s does not look like a snapshot: no %s: %w", dir, Upper, errdefs.ErrInvalidArgument)
	}
	return nil
}

// locked runs fn with a lock of kind how on the snapshot in dir. It fails
// with ErrNotFound if the snapshot is gone or was moved aside to be
// removed while waiting.
func locked(dir string, how int, fn func() error) error {
	path := filepath.Join(dir, LockFile)
	if _, err := os.Stat(filepath.Join(dir, Upper)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no snapshot in %s: %w", dir, errdefs.ErrNotFound)
		}
		return err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		return err
	}
	if same, err := samePath(f, path); err != nil || !same {
		return fmt.Errorf("snapshot %s is being removed: %w", dir, errdefs.ErrNotFound)
	}
	return fn()
}

// samePath says whether path still names the file f has open.
func samePath(f *os.File, path string) (bool, error) {
	var a, b unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &a); err != nil {
		return false, err
	}
	if err := unix.Stat(path, &b); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	return a.Dev == b.Dev && a.Ino == b.Ino, nil
}
