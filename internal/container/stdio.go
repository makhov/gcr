//go:build linux

package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/fifo"
)

// stdio connects the container to containerd's fifos (or a file, or a
// logging binary: openStdio) through anonymous
// pipes. The container only ever sees pipes, which CRIU can checkpoint and
// re-attach on restore; the fifos stay on the shim side for the container's
// whole life. Each incarnation of the container's processes (start, restore,
// rewind) gets fresh output pipes from attach.
type stdio struct {
	// stdin pipe: the read end is handed to every incarnation.
	inR, inW  *os.File
	stdinFifo io.Closer
	// stdinCopied is closed when the stdin copy has reached EOF.
	stdinCopied chan struct{}
	// fifos that output pipes are copied to; nil when not requested.
	stdout, stderr io.Writer

	closers []io.Closer

	mu     sync.Mutex
	out    *os.File // container ends of the current incarnation
	errw   *os.File
	copies sync.WaitGroup
}

// openStdio opens what containerd asked for: fifos (paths), or, as a URI,
// a file both streams go to (file://) or a logging binary that reads them
// (binary://); the container's pipes are copied to either the same way.
// id is the process's, for the logging binary.
func openStdio(ctx context.Context, id, stdin, stdout, stderr string) (_ *stdio, retErr error) {
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
		s.stdinCopied = make(chan struct{})
		go func() {
			_, _ = io.Copy(w, f)
			w.Close()
			close(s.stdinCopied)
		}()
	}

	u, err := url.Parse(stdout)
	if err != nil {
		return nil, fmt.Errorf("stdout %q: %w", stdout, err)
	}
	switch u.Scheme {
	case "", "fifo":
	case "file":
		// Both streams to one file, as containerd's runc shim does.
		if err := os.MkdirAll(filepath.Dir(u.Path), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(u.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		s.closers = append(s.closers, f)
		s.stdout, s.stderr = f, f
		return s, nil
	case "binary", "binary-v2":
		l, err := startLogger(ctx, id, u)
		if err != nil {
			return nil, err
		}
		s.closers = append(s.closers, l)
		s.stdout, s.stderr = l.out, l.err
		return s, nil
	default:
		return nil, fmt.Errorf("stdout %q: unknown scheme %s", stdout, u.Scheme)
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

// closeStdin gives the process EOF on stdin (CloseIO). containerd has
// closed its end of the fifo by then, so the copy is about to reach EOF;
// closing under it would drop what it has not copied yet.
func (s *stdio) closeStdin() {
	if s.stdinCopied == nil {
		return
	}
	select {
	case <-s.stdinCopied:
	case <-time.After(stdinDrainTimeout):
		s.stdinFifo.Close()
		s.inW.Close()
	}
}

// stdinDrainTimeout bounds closeStdin's wait for a writer that never
// closes its end of the stdin fifo.
const stdinDrainTimeout = 5 * time.Second

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

// logger is a logging binary (binary:// stdio, as nerdctl uses): it reads
// stdout on fd 3 and stderr on fd 4, and closes fd 5 (binary-v2: writes a
// byte first) once it is ready, as containerd's runc shim starts it.
type logger struct {
	cmd      *exec.Cmd
	out, err *os.File // the shim's ends
}

// loggerStopTimeout is how long a logger has to flush after SIGTERM,
// containerd's runc shim's.
const loggerStopTimeout = 12 * time.Second

func startLogger(ctx context.Context, id string, u *url.URL) (_ *logger, retErr error) {
	var args []string
	for k, vs := range u.Query() {
		args = append(args, k)
		if len(vs) > 0 {
			args = append(args, vs[0])
		}
	}
	ns, _ := namespaces.Namespace(ctx)
	cmd := exec.Command(u.Path, args...)
	cmd.Env = []string{"CONTAINER_ID=" + id, "CONTAINER_NAMESPACE=" + ns}

	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	pipe := func() (r, w *os.File, err error) {
		if r, w, err = os.Pipe(); err == nil {
			files = append(files, r, w)
		}
		return r, w, err
	}
	outR, outW, err := pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := pipe()
	if err != nil {
		return nil, err
	}
	readyR, readyW, err := pipe()
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{outR, errR, readyW}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("logging binary %s: %w", u.Path, err)
	}
	l := &logger{cmd: cmd, out: outW, err: errW}
	defer func() {
		if retErr != nil {
			l.Close()
		}
	}()
	readyW.Close()
	b := make([]byte, 1)
	n, err := readyR.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("logging binary %s: %w", u.Path, err)
	}
	if u.Scheme == "binary-v2" && n == 0 {
		return nil, fmt.Errorf("logging binary %s exited before it was ready", u.Path)
	}
	files = []*os.File{outR, errR, readyR} // the shim keeps only its write ends
	return l, nil
}

// Close ends the streams, then the logger: SIGTERM, and SIGKILL if it has
// not exited after loggerStopTimeout.
func (l *logger) Close() error {
	l.out.Close()
	l.err.Close()
	if err := l.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = l.cmd.Process.Kill()
	}
	done := make(chan struct{})
	go func() { _ = l.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(loggerStopTimeout):
		_ = l.cmd.Process.Kill()
		<-done
	}
	return nil
}
