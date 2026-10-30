//go:build linux

package timetravel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/supervisor"
)

// Snapshotter is the container side of time travel.
type Snapshotter interface {
	// Running reports whether the container's process has started.
	Running() bool
	// Point writes a checkpoint of the running container into dir. With
	// parent, the directory of the last point, it may dump only the
	// memory written since then (falling back to a full one).
	Point(ctx context.Context, dir, parent string) error
	// Rewind replaces the running container with the checkpoint in dir.
	Rewind(ctx context.Context, dir string) error
	// Pids lists the container's processes (host pids).
	Pids() ([]int, error)
}

// Recorder takes points on triggers and rewinds to them.
//
// A point is taken just after the triggering execve, before the new program
// runs its first instruction (see stopAfterExec).
// Taking it before execve is not possible in general: execs usually come
// from vfork children (posix_spawn, busybox sh, Go os/exec), whose parent
// waits uninterruptibly and whose memory is shared until the exec, and CRIU
// can dump neither. The task stays stopped in the point and is continued
// after a snapshot or a rewind.
type Recorder struct {
	policy   *Policy
	timeline *Timeline

	mu   sync.Mutex
	snap Snapshotter
	last time.Time
	// The container's first execve is libcontainer's init executing the
	// container's process; it is not a point.
	seenInit bool
	// The last point, and how many points its chain has: up to
	// fullEvery, a point is incremental on the last one.
	lastID, chain int
}

// fullEvery is how long a chain of points gets before a full one starts
// the next: old points are deleted a chain at a time, so up to this many
// more than Keep are kept.
const fullEvery = 8

// NewRecorder keeps its timeline in stateDir. Its Handle answers the
// policy's trigger calls on the container's supervisor.
func NewRecorder(policy *Policy, stateDir string) (*Recorder, error) {
	tl, err := NewTimeline(filepath.Join(stateDir, "points"), policy.Keep)
	if err != nil {
		return nil, err
	}
	return &Recorder{policy: policy, timeline: tl}, nil
}

// Attach sets the container once it exists; triggers before that pass.
func (r *Recorder) Attach(s Snapshotter) {
	r.mu.Lock()
	r.snap = s
	r.mu.Unlock()
}

func (r *Recorder) Points() []Point { return r.timeline.Points() }

// Handle is the supervisor handler for the policy's trigger calls.
func (r *Recorder) Handle(ctx context.Context, c *supervisor.Call) {
	t, ok := readTrigger(c)
	if !ok {
		return
	}
	r.handle(ctx, t, c.Continue)
}

// Trigger is a held exec that may take a point.
type Trigger struct {
	Pid     int
	Syscall string
	Path    string
	Argv    []string
}

// readTrigger reads execve(path, argv, envp) / execveat(dirfd, path, argv,
// envp, flags) from the caller.
func readTrigger(c *supervisor.Call) (Trigger, bool) {
	t := Trigger{Pid: c.Pid, Syscall: c.Syscall}
	pathArg, argvArg := c.Args[0], c.Args[1]
	if c.Syscall == "execveat" {
		pathArg, argvArg = c.Args[1], c.Args[2]
	}
	var err error
	if t.Path, err = c.ReadString(pathArg, 4096); err != nil {
		return t, false
	}
	for i := uint64(0); i < 32 && argvArg != 0; i++ {
		p, err := c.ReadUint64(argvArg + i*8)
		if err != nil || p == 0 {
			break
		}
		a, err := c.ReadString(p, 4096)
		if err != nil {
			break
		}
		t.Argv = append(t.Argv, a)
	}
	return t, c.Valid()
}

func (r *Recorder) handle(ctx context.Context, t Trigger, allow func()) {
	r.mu.Lock()
	snap := r.snap
	due := snap != nil && r.seenInit && time.Since(r.last) >= r.policy.Interval
	r.seenInit = true
	r.mu.Unlock()
	// An exec that cannot find its file fails: it is not a point, and
	// does not count against the interval.
	if !due || !snap.Running() || (t.Syscall == "execve" && !executable(t.Pid, t.Path)) {
		allow()
		return
	}
	r.mu.Lock()
	// Count attempts, not successes, so a failing point cannot hold the
	// container in a retry loop.
	r.last = time.Now()
	r.mu.Unlock()

	nspid, err := nsPid(t.Pid)
	if err != nil {
		allow()
		return
	}
	if err := stopAfterExec(t.Pid, allow, 2*time.Second); err != nil {
		if !errors.Is(err, errNoExec) {
			log.G(ctx).WithError(err).Warn("time travel: stop task after exec")
		}
		return
	}
	defer func() { _ = unix.Kill(t.Pid, unix.SIGCONT) }()
	if err := waitStopped(t.Pid, 2*time.Second); err != nil {
		log.G(ctx).WithError(err).Warn("time travel: task did not stop after exec")
		return
	}

	p := Point{Syscall: t.Syscall, Path: t.Path, Argv: t.Argv, StoppedPid: nspid}
	if _, err := r.point(ctx, snap, p); err != nil {
		log.G(ctx).WithError(err).Error("time travel")
	}
}

// Take takes a point now, with the container frozen rather than a task
// stopped at an exec; label says what it was taken before. A rewind to it
// lets every process go on from where it was frozen.
func (r *Recorder) Take(ctx context.Context, label string) (Point, error) {
	r.mu.Lock()
	snap := r.snap
	r.mu.Unlock()
	if snap == nil || !snap.Running() {
		return Point{}, fmt.Errorf("container is not running: %w", errdefs.ErrFailedPrecondition)
	}
	return r.point(ctx, snap, Point{Label: label})
}

// point dumps the container as the next point on the timeline, p filled in
// with its id, time and parent.
func (r *Recorder) point(ctx context.Context, snap Snapshotter, p Point) (Point, error) {
	id, dir, err := r.timeline.Reserve()
	if err != nil {
		return p, fmt.Errorf("reserve point: %w", err)
	}
	r.mu.Lock()
	parentID := 0
	if r.lastID != 0 && r.chain < fullEvery {
		parentID = r.lastID
	}
	r.mu.Unlock()
	parent := ""
	if parentID != 0 {
		parent = r.timeline.Dir(parentID)
	}
	start := time.Now()
	err = snap.Point(ctx, dir, parent)
	if err != nil && parent != "" {
		// CRIU can refuse an incremental dump (a page it takes for
		// unchanged is not in the parent's images); a full one holds
		// everything, and the task is still stopped where it was.
		log.G(ctx).WithError(err).Warn("time travel: incremental point failed, taking a full one")
		parentID = 0
		if err = os.RemoveAll(dir); err == nil {
			err = os.MkdirAll(filepath.Join(dir, "images"), 0o700)
		}
		if err == nil {
			err = snap.Point(ctx, dir, "")
		}
	}
	if err != nil {
		r.timeline.Discard(id)
		r.mu.Lock()
		r.lastID = 0 // the next starts a chain
		r.mu.Unlock()
		return p, fmt.Errorf("snapshot: %w", err)
	}
	// The container falls back to a full dump when it cannot do an
	// incremental one; CRIU links "parent" in the images when it did.
	if _, err := os.Lstat(filepath.Join(dir, "images", "parent")); err != nil {
		parentID = 0
	}
	p.ID, p.Time, p.Parent = id, start, parentID
	if err := r.timeline.Commit(p); err != nil {
		r.timeline.Discard(id)
		return p, fmt.Errorf("commit point: %w", err)
	}
	r.mu.Lock()
	if parentID == 0 {
		r.chain = 1
	} else {
		r.chain++
	}
	r.lastID = id
	r.mu.Unlock()
	log.G(ctx).WithField("point", id).WithField("took", time.Since(start)).Debug("time travel: point")
	return p, nil
}

// Rewind restores the container to point id and continues the task that was
// stopped in it, if any.
func (r *Recorder) Rewind(ctx context.Context, id int) error {
	p, err := r.timeline.Get(id)
	if err != nil {
		return err
	}
	r.mu.Lock()
	snap := r.snap
	r.mu.Unlock()
	if snap == nil {
		return errors.New("container not attached")
	}
	if err := snap.Rewind(ctx, r.timeline.Dir(id)); err != nil {
		return err
	}
	r.mu.Lock()
	r.last = time.Now()
	r.lastID = 0 // the restored memory has no tracking: start a chain
	r.mu.Unlock()

	if p.StoppedPid == 0 {
		return nil // taken frozen: the restore left everything running
	}
	pids, err := snap.Pids()
	if err != nil {
		return fmt.Errorf("continue rewound task: %w", err)
	}
	for _, pid := range pids {
		if n, err := nsPid(pid); err == nil && n == p.StoppedPid {
			return unix.Kill(pid, unix.SIGCONT)
		}
	}
	return fmt.Errorf("continue rewound task: pid %d not found in the container", p.StoppedPid)
}

func (r *Recorder) Close() error {
	return r.timeline.Remove()
}

// nsPid returns pid as seen in its innermost pid namespace (the container's).
func nsPid(pid int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "NSpid:"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				break
			}
			return strconv.Atoi(f[len(f)-1])
		}
	}
	return 0, fmt.Errorf("no NSpid for pid %d", pid)
}

// waitStopped waits until pid is in the stopped (T) state.
func waitStopped(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return err
		}
		// The state follows the parenthesised command name.
		if i := strings.LastIndexByte(string(b), ')'); i >= 0 && i+2 < len(b) && b[i+2] == 'T' {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pid %d did not stop", pid)
		}
		time.Sleep(time.Millisecond)
	}
}
