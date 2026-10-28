//go:build linux

// Package supervisor answers a container's seccomp user notifications:
// system calls its filter hands to user space (time-travel triggers,
// mounts). One supervisor serves one container; libcontainer allows a
// single listener per container.
package supervisor

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/containerd/log"
	libseccomp "github.com/seccomp/libseccomp-golang"
	"golang.org/x/sys/unix"
)

// Call is one held system call. A handler either lets it run (Continue)
// or answers it itself (Reply); if it does neither, the call continues.
type Call struct {
	Pid     int
	Syscall string
	Args    [6]uint64

	fd   libseccomp.ScmpFd
	id   uint64
	ctx  context.Context
	once sync.Once
}

// Continue lets the call run in the kernel as the caller made it. Its
// arguments may have changed since a handler read them; use it only where
// that does not matter.
func (c *Call) Continue() {
	c.respond(&libseccomp.ScmpNotifResp{ID: c.id, Flags: libseccomp.NotifRespFlagContinue})
}

// Reply answers the call without running it: val on success, or errno.
func (c *Call) Reply(val uint64, errno syscall.Errno) {
	if errno != 0 {
		c.respond(&libseccomp.ScmpNotifResp{ID: c.id, Error: -int32(errno)})
		return
	}
	c.respond(&libseccomp.ScmpNotifResp{ID: c.id, Val: val})
}

func (c *Call) respond(r *libseccomp.ScmpNotifResp) {
	c.once.Do(func() {
		// ENOENT: the task was killed or interrupted while its call was held.
		if err := libseccomp.NotifRespond(c.fd, r); err != nil && !errors.Is(err, unix.ENOENT) {
			log.G(c.ctx).WithError(err).Warn("seccomp notify respond")
		}
	})
}

// Valid reports whether the call is still pending: reads of the caller's
// memory are only meaningful while it is (the pid may be reused otherwise).
func (c *Call) Valid() bool {
	return libseccomp.NotifIDValid(c.fd, c.id) == nil
}

// ReadString reads a NUL-terminated string of at most max bytes from the
// caller's memory. The copy is what a handler must act on.
func (c *Call) ReadString(addr uint64, max int) (string, error) {
	if addr == 0 {
		return "", nil
	}
	buf := make([]byte, max+1)
	n, err := c.readMem(addr, buf)
	if n == 0 && err != nil {
		return "", err
	}
	i := bytes.IndexByte(buf[:n], 0)
	if i < 0 {
		return "", fmt.Errorf("string at %#x longer than %d bytes", addr, max)
	}
	if !c.Valid() {
		return "", errors.New("call no longer pending")
	}
	return string(buf[:i]), nil
}

// ReadUint64 reads a little-endian word (the byte order of every arch gcr
// supports) from the caller's memory.
func (c *Call) ReadUint64(addr uint64) (uint64, error) {
	var b [8]byte
	if _, err := c.readMem(addr, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

func (c *Call) readMem(addr uint64, buf []byte) (int, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", c.Pid))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.ReadAt(buf, int64(addr))
}

// Handler answers one held call.
type Handler func(ctx context.Context, c *Call)

// Supervisor receives the container's listeners: from libcontainer when the
// container starts (HandoffPath) and from CRIU --seccomp-notify-socket
// after a restore (RestorePath), and dispatches calls to handlers.
type Supervisor struct {
	HandoffPath string
	RestorePath string

	handlers map[string]Handler
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	lns      []net.Listener
}

// New listens in sockDir and dispatches calls by system call name.
func New(ctx context.Context, sockDir string, handlers map[string]Handler) (*Supervisor, error) {
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Supervisor{
		HandoffPath: filepath.Join(sockDir, "handoff.sock"),
		RestorePath: filepath.Join(sockDir, "restore.sock"),
		handlers:    handlers,
		ctx:         ctx,
		cancel:      cancel,
	}
	// libcontainer connects with SOCK_STREAM, CRIU with SOCK_SEQPACKET.
	for _, l := range []struct{ path, network string }{
		{s.HandoffPath, "unix"},
		{s.RestorePath, "unixpacket"},
	} {
		_ = os.Remove(l.path)
		ln, err := net.Listen(l.network, l.path)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("listen %s: %w", l.path, err)
		}
		s.lns = append(s.lns, ln)
		s.wg.Add(1)
		go s.accept(ln.(*net.UnixListener))
	}
	return s, nil
}

// WriteCRIUConfig writes a CRIU config file that hands restored listeners
// to this supervisor. libcontainer restores through CRIU's RPC, which has
// no field for --seccomp-notify-socket; org.criu.config names the file.
func (s *Supervisor) WriteCRIUConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("seccomp-notify-socket %s\n", s.RestorePath)), 0o600)
}

func (s *Supervisor) Close() {
	s.cancel()
	for _, ln := range s.lns {
		ln.Close()
	}
	_ = os.Remove(s.HandoffPath)
	_ = os.Remove(s.RestorePath)
	s.wg.Wait()
}

func (s *Supervisor) accept(ln *net.UnixListener) {
	defer s.wg.Done()
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			// One connection may carry several listeners (one per restored task).
			for {
				fd, err := recvFd(conn)
				if err != nil {
					return
				}
				s.wg.Add(1)
				go s.serve(fd)
			}
		}()
	}
}

// recvFd reads one message carrying a file descriptor; the payload
// (libcontainer's state JSON or CRIU's thread id) is not needed.
func recvFd(conn *net.UnixConn) (int, error) {
	buf := make([]byte, 64*1024)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, err
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return -1, errors.New("no file descriptor in message")
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) == 0 {
		return -1, errors.New("no file descriptor in message")
	}
	for _, extra := range fds[1:] {
		unix.Close(extra)
	}
	return fds[0], nil
}

// serve answers notifications on one listener until its tasks are gone.
func (s *Supervisor) serve(fd int) {
	defer s.wg.Done()
	defer unix.Close(fd)
	sfd := libseccomp.ScmpFd(fd)
	pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for s.ctx.Err() == nil {
		// Closing a listener while its tasks live makes every notified call
		// fail with ENOSYS, so leave only on POLLHUP or a hard error.
		n, err := unix.Poll(pfd, 500)
		if errors.Is(err, unix.EINTR) || (err == nil && n == 0) {
			continue
		}
		if err != nil {
			log.G(s.ctx).WithError(err).Error("seccomp listener poll")
			return
		}
		if pfd[0].Revents&unix.POLLIN == 0 {
			return // POLLHUP: every task using this filter has exited
		}
		req, err := libseccomp.NotifReceive(sfd)
		if err != nil {
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EINTR) {
				continue
			}
			log.G(s.ctx).WithError(err).Error("seccomp listener receive")
			return
		}
		c := &Call{Pid: int(req.Pid), fd: sfd, id: req.ID, ctx: s.ctx}
		copy(c.Args[:], req.Data.Args)
		name, err := req.Data.Syscall.GetName()
		if h, ok := s.handlers[name]; err == nil && ok {
			c.Syscall = name
			h(s.ctx, c)
		}
		c.Continue() // no-op if the handler answered
	}
}
