//go:build linux

package container

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// Exec is an additional process run in a running container (containerd's
// exec processes, kubectl exec).
type Exec struct {
	ID string

	c      *Container
	spec   *specs.Process
	pipes  *stdio
	tty    *ttyIO
	onExit func(*Exec)

	mu         sync.Mutex
	status     Status
	pid        int
	exitStatus uint32
	exitedAt   time.Time
	exited     chan struct{}
}

// AddExec prepares an exec process; Start runs it.
func (c *Container) AddExec(ctx context.Context, id string, spec *specs.Process, stdin, stdout, stderr string, onExit func(*Exec)) (_ *Exec, retErr error) {
	if c.Status() != Running {
		return nil, fmt.Errorf("container is not running: %w", errdefs.ErrFailedPrecondition)
	}
	e := &Exec{ID: id, c: c, spec: spec, onExit: onExit, exited: make(chan struct{})}
	var err error
	if spec.Terminal {
		e.tty, err = openTTY(ctx, stdin, stdout)
	} else {
		e.pipes, err = openStdio(ctx, id, stdin, stdout, stderr)
	}
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.execs == nil {
		c.execs = map[string]*Exec{}
	}
	if _, ok := c.execs[id]; ok {
		e.closeIO()
		return nil, fmt.Errorf("exec %s: %w", id, errdefs.ErrAlreadyExists)
	}
	c.execs[id] = e
	return e, nil
}

// Exec returns a registered exec process.
func (c *Container) Exec(id string) (*Exec, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.execs[id]
	if !ok {
		return nil, fmt.Errorf("exec %s: %w", id, errdefs.ErrNotFound)
	}
	return e, nil
}

func (e *Exec) Start(ctx context.Context) error {
	if e.Status() != Created {
		return fmt.Errorf("exec %s already started: %w", e.ID, errdefs.ErrFailedPrecondition)
	}
	proc, err := newProcess(e.spec)
	if err != nil {
		return err
	}
	if e.tty != nil {
		e.tty.setup(proc, e.spec.ConsoleSize)
	} else if proc.Stdin, proc.Stdout, proc.Stderr, err = e.pipes.attach(e.c.ioOwner); err != nil {
		return err
	}

	e.c.op.Lock()
	err = e.c.lc.Run(proc)
	e.c.op.Unlock()
	if e.pipes != nil {
		e.pipes.detach()
	}
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if e.tty != nil {
		if err := e.tty.started(); err != nil {
			_ = proc.Signal(unix.SIGKILL)
			return err
		}
	}
	pid, err := proc.Pid()
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.pid, e.status = pid, Running
	e.mu.Unlock()

	go func() {
		st, _ := proc.Wait()
		if e.tty != nil {
			e.tty.wait()
		} else {
			e.pipes.wait()
		}
		// containerd reads the fifos until EOF before it deletes the exec.
		e.closeIO()
		e.mu.Lock()
		e.status = Stopped
		e.exitStatus = exitStatus(st)
		e.exitedAt = time.Now()
		e.mu.Unlock()
		close(e.exited)
		if e.onExit != nil {
			e.onExit(e)
		}
	}()
	return nil
}

func (e *Exec) Kill(sig syscall.Signal) error {
	e.mu.Lock()
	pid, status := e.pid, e.status
	e.mu.Unlock()
	if status != Running {
		return fmt.Errorf("exec %s is not running: %w", e.ID, errdefs.ErrNotFound)
	}
	if err := unix.Kill(pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

// Delete forgets a stopped (or never started) exec process.
func (e *Exec) Delete() error {
	if e.Status() == Running {
		return fmt.Errorf("exec %s is running: %w", e.ID, errdefs.ErrFailedPrecondition)
	}
	e.c.mu.Lock()
	delete(e.c.execs, e.ID)
	e.c.mu.Unlock()
	e.closeIO()
	return nil
}

func (e *Exec) Resize(width, height uint32) error {
	if e.tty == nil {
		return fmt.Errorf("exec %s has no terminal: %w", e.ID, errdefs.ErrFailedPrecondition)
	}
	return e.tty.resize(width, height)
}

func (e *Exec) CloseStdin() {
	if e.tty != nil {
		e.tty.closeStdin()
	} else {
		e.pipes.closeStdin()
	}
}

func (e *Exec) closeIO() {
	if e.tty != nil {
		e.tty.Close()
	} else {
		e.pipes.Close()
	}
}

func (e *Exec) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

func (e *Exec) Pid() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pid
}

func (e *Exec) Exit() (uint32, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exitStatus, e.exitedAt
}

func (e *Exec) Exited() <-chan struct{} { return e.exited }

// Spec is the exec's process spec.
func (e *Exec) Spec() *specs.Process { return e.spec }
