//go:build linux

package container

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/containerd/fifo"
)

// stdio connects the container to containerd's fifos through anonymous
// pipes. The container only ever sees pipes, which CRIU can checkpoint and
// re-attach on restore; the fifos stay on the shim side for the container's
// whole life. Each incarnation of the container's processes (start, restore,
// rewind) gets fresh output pipes from attach.
type stdio struct {
	// stdin pipe: the read end is handed to every incarnation.
	inR, inW  *os.File
	stdinFifo io.Closer
	// fifos that output pipes are copied to; nil when not requested.
	stdout, stderr io.Writer

	closers []io.Closer

	mu     sync.Mutex
	out    *os.File // container ends of the current incarnation
	errw   *os.File
	copies sync.WaitGroup
}

func openStdio(ctx context.Context, stdin, stdout, stderr string) (_ *stdio, retErr error) {
	s := &stdio{}
	defer func() {
		if retErr != nil {
			s.Close()
		}
	}()

	if stdin != "" {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		s.inR, s.inW = r, w
		s.closers = append(s.closers, r, w)
		f, err := fifo.OpenFifo(ctx, stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("open stdin fifo: %w", err)
		}
		s.closers = append(s.closers, f)
		s.stdinFifo = f
		go func() {
			_, _ = io.Copy(w, f)
			w.Close()
		}()
	}

	for _, out := range []struct {
		path string
		dst  *io.Writer
	}{
		{stdout, &s.stdout},
		{stderr, &s.stderr},
	} {
		if out.path == "" {
			continue
		}
		fw, err := fifo.OpenFifo(ctx, out.path, syscall.O_WRONLY, 0)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", out.path, err)
		}
		// Keep a reader open so writes never fail with EPIPE while
		// containerd's reader is not attached yet.
		fr, err := fifo.OpenFifo(ctx, out.path, syscall.O_RDONLY, 0)
		if err != nil {
			fw.Close()
			return nil, fmt.Errorf("open %s: %w", out.path, err)
		}
		s.closers = append(s.closers, fw, fr)
		*out.dst = fw
	}
	return s, nil
}

// attach creates output pipes for a new incarnation and starts copying them
// to the fifos. It returns the files to give the init process; nil means
// /dev/null. owner, if not 0, is the host ID of the container's root in its
// user namespace: the container's pipe ends become its, so the process can
// reopen them (/dev/stdout, /proc/self/fd/1), as runc does.
func (s *stdio) attach(owner int) (in io.Reader, out, errw io.Writer, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inR != nil {
		if err := chown(s.inR, owner); err != nil {
			return nil, nil, nil, err
		}
		in = s.inR
	}
	for _, o := range []struct {
		fifo io.Writer
		end  **os.File
	}{
		{s.stdout, &s.out},
		{s.stderr, &s.errw},
	} {
		if o.fifo == nil {
			continue
		}
		r, w, err := os.Pipe()
		if err != nil {
			return nil, nil, nil, err
		}
		if err := chown(w, owner); err != nil {
			r.Close()
			w.Close()
			return nil, nil, nil, err
		}
		*o.end = w
		s.copies.Add(1)
		go func(fifo io.Writer) {
			defer s.copies.Done()
			defer r.Close()
			_, _ = io.Copy(fifo, r)
		}(o.fifo)
	}
	if s.out != nil {
		out = s.out
	}
	if s.errw != nil {
		errw = s.errw
	}
	return in, out, errw, nil
}

func chown(f *os.File, owner int) error {
	if owner == 0 {
		return nil
	}
	return f.Chown(owner, owner)
}

// detach drops the shim's copies of the incarnation's output ends, so the
// copies end when the container's processes close theirs.
func (s *stdio) detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range []*os.File{s.out, s.errw} {
		if f != nil {
			f.Close()
		}
	}
	s.out, s.errw = nil, nil
}

// closeStdin gives the process EOF on stdin (CloseIO).
func (s *stdio) closeStdin() {
	if s.stdinFifo != nil {
		s.stdinFifo.Close()
	}
	if s.inW != nil {
		s.inW.Close()
	}
}

// wait blocks until all container output has been copied to the fifos.
func (s *stdio) wait() {
	s.copies.Wait()
}

func (s *stdio) Close() {
	s.detach()
	for _, c := range s.closers {
		c.Close()
	}
}
