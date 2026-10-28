//go:build linux

package timetravel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/containerd/errdefs"
)

// Point is one checkpoint on a container's timeline.
type Point struct {
	ID      int       `json:"id"`
	Time    time.Time `json:"time"`
	Syscall string    `json:"syscall"`
	Path    string    `json:"path"`
	Argv    []string  `json:"argv,omitempty"`
	// StoppedPid is the task stopped just after its execve, as a pid in
	// the container's pid namespace; it is continued after a rewind.
	StoppedPid int `json:"stoppedPid"`
}

// Timeline stores points as directories: <root>/<id>/{images,upper,point.json}.
type Timeline struct {
	root string
	keep int

	mu     sync.Mutex
	points []Point
	next   int
}

func NewTimeline(root string, keep int) (*Timeline, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Timeline{root: root, keep: keep, next: 1}, nil
}

// Reserve returns the id and directory for the next point.
func (t *Timeline) Reserve() (int, string, error) {
	t.mu.Lock()
	id := t.next
	t.next++
	t.mu.Unlock()
	dir := t.Dir(id)
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o700); err != nil {
		return 0, "", err
	}
	return id, dir, nil
}

// Commit records a point whose directory is complete and prunes old points.
func (t *Timeline) Commit(p Point) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(t.Dir(p.ID), "point.json"), b, 0o600); err != nil {
		return err
	}
	t.mu.Lock()
	t.points = append(t.points, p)
	var drop []Point
	if len(t.points) > t.keep {
		drop = slices.Clone(t.points[:len(t.points)-t.keep])
		t.points = slices.Clone(t.points[len(t.points)-t.keep:])
	}
	t.mu.Unlock()
	for _, d := range drop {
		_ = os.RemoveAll(t.Dir(d.ID))
	}
	return nil
}

// Discard removes a reserved point that was never committed.
func (t *Timeline) Discard(id int) {
	_ = os.RemoveAll(t.Dir(id))
}

func (t *Timeline) Points() []Point {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.points)
}

func (t *Timeline) Get(id int) (Point, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range t.points {
		if p.ID == id {
			return p, nil
		}
	}
	return Point{}, fmt.Errorf("point %d: %w", id, errdefs.ErrNotFound)
}

func (t *Timeline) Dir(id int) string {
	return filepath.Join(t.root, strconv.Itoa(id))
}

func (t *Timeline) Remove() error {
	return os.RemoveAll(t.root)
}
