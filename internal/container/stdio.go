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
// re-attach on restore; the fifos stay on the shim side.
type stdio struct {
	// container ends, handed to the init process and closed after start
	in, out, errw *os.File
	// shim ends
	inW, outR, errR *os.File

	fifos  []io.Closer
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
		s.in, s.inW = r, w
		f, err := fifo.OpenFifo(ctx, stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("open stdin fifo: %w", err)
		}
		s.fifos = append(s.fifos, f)
		go func() {
			_, _ = io.Copy(w, f)
			w.Close()
		}()
	}

	for _, out := range []struct {
		path      string
		container **os.File
		shim      **os.File
	}{
		{stdout, &s.out, &s.outR},
		{stderr, &s.errw, &s.errR},
	} {
		if out.path == "" {
			continue
		}
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		*out.container, *out.shim = w, r
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
		s.fifos = append(s.fifos, fw, fr)
		s.copies.Add(1)
		go func() {
			defer s.copies.Done()
			_, _ = io.Copy(fw, r)
		}()
	}
	return s, nil
}

// containerSide returns the files for the init process; nil means /dev/null.
func (s *stdio) containerSide() (io.Reader, io.Writer, io.Writer) {
	var in io.Reader
	var out, errw io.Writer
	if s.in != nil {
		in = s.in
	}
	if s.out != nil {
		out = s.out
	}
	if s.errw != nil {
		errw = s.errw
	}
	return in, out, errw
}

// closeContainerSide drops the shim's copies of the container ends, so the
// output copies end when the container's processes close theirs.
func (s *stdio) closeContainerSide() {
	for _, f := range []*os.File{s.in, s.out, s.errw} {
		if f != nil {
			f.Close()
		}
	}
	s.in, s.out, s.errw = nil, nil, nil
}

// wait blocks until all container output has been copied to the fifos.
func (s *stdio) wait() {
	s.copies.Wait()
}

func (s *stdio) Close() {
	s.closeContainerSide()
	for _, f := range []*os.File{s.inW, s.outR, s.errR} {
		if f != nil {
			f.Close()
		}
	}
	for _, f := range s.fifos {
		f.Close()
	}
}
