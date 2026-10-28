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

	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// Snapshotter is the container side of time travel.
type Snapshotter interface {
	// Running reports whether the container's process has started.
	Running() bool
	// Snapshot writes a checkpoint of the running container into dir.
	Snapshot(ctx context.Context, dir string) error
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
	sup      *Supervisor
	criuConf string

	mu   sync.Mutex
	snap Snapshotter
	last time.Time
	// The container's first execve is libcontainer's init executing the
	// container's process; it is not a point.
	seenInit bool
}

// NewRecorder starts the supervisor. stateDir holds the timeline and the CRIU
// config; sockDir holds the supervisor sockets (kept short for sun_path).
func NewRecorder(ctx context.Context, policy *Policy, stateDir, sockDir string) (*Recorder, error) {
	tl, err := NewTimeline(filepath.Join(stateDir, "points"), policy.Keep)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return nil, err
	}
	r := &Recorder{policy: policy, timeline: tl}
	r.sup, err = NewSupervisor(ctx, filepath.Join(sockDir, "handoff.sock"), filepath.Join(sockDir, "restore.sock"), r.handle)
	if err != nil {
		return nil, err
	}
	// libcontainer restores through CRIU's RPC, which has no field for our
	// option; a CRIU config file named by org.criu.config carries it.
	r.criuConf = filepath.Join(stateDir, "criu.conf")
	conf := fmt.Sprintf("seccomp-notify-socket %s\n", r.sup.RestorePath)
	if err := os.WriteFile(r.criuConf, []byte(conf), 0o600); err != nil {
		r.sup.Close()
		return nil, err
	}
	return r, nil
}

// HandoffPath is the seccomp listener path to put in the container's spec.
func (r *Recorder) HandoffPath() string { return r.sup.HandoffPath }

// CRIUConfig is the CRIU configuration file for this container's checkpoints
// and restores.
func (r *Recorder) CRIUConfig() string { return r.criuConf }

// Attach sets the container once it exists; triggers before that pass.
func (r *Recorder) Attach(s Snapshotter) {
	r.mu.Lock()
	r.snap = s
	r.mu.Unlock()
}

func (r *Recorder) Points() []Point { return r.timeline.Points() }

func (r *Recorder) handle(ctx context.Context, t Trigger, allow func()) {
	r.mu.Lock()
	snap := r.snap
	take := snap != nil && r.seenInit && time.Since(r.last) >= r.policy.Interval
	r.seenInit = true
	if take {
		// Count attempts, not successes, so a failing point cannot
		// hold the container in a retry loop.
		r.last = time.Now()
	}
	r.mu.Unlock()
	if !take || !snap.Running() {
		allow()
		return
	}

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

	id, dir, err := r.timeline.Reserve()
	if err != nil {
		log.G(ctx).WithError(err).Error("time travel: reserve point")
		return
	}
	start := time.Now()
	if err := snap.Snapshot(ctx, dir); err != nil {
		r.timeline.Discard(id)
		log.G(ctx).WithError(err).Error("time travel: snapshot")
		return
	}
	p := Point{ID: id, Time: start, Syscall: t.Syscall, Path: t.Path, Argv: t.Argv, StoppedPid: nspid}
	if err := r.timeline.Commit(p); err != nil {
		r.timeline.Discard(id)
		log.G(ctx).WithError(err).Error("time travel: commit point")
		return
	}
	log.G(ctx).WithField("point", id).WithField("took", time.Since(start)).Debug("time travel: point")
}

// Rewind restores the container to point id and continues the task that was
// stopped in it.
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
	r.mu.Unlock()

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
	r.sup.Close()
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
