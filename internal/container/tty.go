//go:build linux

package container

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/containerd/console"
	"github.com/containerd/fifo"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/utils"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// ttyIO connects a process that has a terminal to containerd's fifos: the
// process's init sends the pty master over a socket, and the shim copies
// stdin into it and its output to the stdout fifo.
type ttyIO struct {
	parent, child *os.File // console socket pair
	recv          chan error

	stdin  io.ReadCloser // fifo, nil if no stdin
	stdout io.Writer     // fifo, nil if no stdout

	mu      sync.Mutex
	console console.Console
	closers []io.Closer
	copies  sync.WaitGroup
}

func openTTY(ctx context.Context, stdin, stdout string) (_ *ttyIO, retErr error) {
	parent, child, err := utils.NewSockPair("console")
	if err != nil {
		return nil, err
	}
	t := &ttyIO{parent: parent, child: child, recv: make(chan error, 1), closers: []io.Closer{parent, child}}
	defer func() {
		if retErr != nil {
			t.Close()
		}
	}()
	if stdin != "" {
		f, err := fifo.OpenFifo(ctx, stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("open stdin fifo: %w", err)
		}
		t.stdin = f
		t.closers = append(t.closers, f)
	}
	if stdout != "" {
		fw, err := fifo.OpenFifo(ctx, stdout, syscall.O_WRONLY, 0)
		if err != nil {
			return nil, fmt.Errorf("open stdout fifo: %w", err)
		}
		fr, err := fifo.OpenFifo(ctx, stdout, syscall.O_RDONLY, 0)
		if err != nil {
			fw.Close()
			return nil, fmt.Errorf("open stdout fifo: %w", err)
		}
		t.stdout = fw
		t.closers = append(t.closers, fw, fr)
	}
	go func() { t.recv <- t.receive() }()
	return t, nil
}

// receive takes the pty master from the container and starts the copies.
func (t *ttyIO) receive() error {
	f, err := utils.RecvFile(t.parent)
	if err != nil {
		return fmt.Errorf("receive console: %w", err)
	}
	cons, err := console.ConsoleFromFile(f)
	if err != nil {
		f.Close()
		return err
	}
	if err := console.ClearONLCR(cons.Fd()); err != nil {
		cons.Close()
		return err
	}
	t.mu.Lock()
	t.console = cons
	t.closers = append(t.closers, cons)
	t.mu.Unlock()
	if t.stdin != nil {
		go func() { _, _ = io.Copy(cons, t.stdin) }()
	}
	t.copies.Add(1)
	go func() {
		defer t.copies.Done()
		// Read ends with EIO once every process holding the terminal exits.
		if t.stdout != nil {
			_, _ = io.Copy(t.stdout, cons)
		} else {
			_, _ = io.Copy(io.Discard, cons)
		}
	}()
	return nil
}

// setup makes p create a terminal and send its master to us.
func (t *ttyIO) setup(p *libcontainer.Process, size *specs.Box) {
	p.Stdin, p.Stdout, p.Stderr = nil, nil, nil
	p.ConsoleSocket = t.child
	if size != nil {
		p.ConsoleHeight, p.ConsoleWidth = uint16(size.Height), uint16(size.Width)
	}
}

// started waits for the console once the process is running.
func (t *ttyIO) started() error {
	t.child.Close()
	return <-t.recv
}

func (t *ttyIO) resize(width, height uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.console == nil {
		return fmt.Errorf("no console yet")
	}
	return t.console.Resize(console.WinSize{Width: uint16(width), Height: uint16(height)})
}

func (t *ttyIO) closeStdin() {
	if t.stdin != nil {
		t.stdin.Close()
	}
}

func (t *ttyIO) wait() { t.copies.Wait() }

func (t *ttyIO) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.closers {
		c.Close()
	}
}
