//go:build linux

package timetravel

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/containerd/log"
	libseccomp "github.com/seccomp/libseccomp-golang"
	"golang.org/x/sys/unix"
)

// Trigger is a held system call that may take a point.
type Trigger struct {
	Pid     int
	Syscall string
	Path    string
	Argv    []string
}

// Handler gets each held call. It must call allow exactly once to let the
// call continue; it may keep working after that.
type Handler func(ctx context.Context, t Trigger, allow func())

// Supervisor answers seccomp notifications for one container. Listeners come
// from libcontainer when the container starts (HandoffPath) and from CRIU
// --seccomp-notify-socket after a restore (RestorePath).
type Supervisor struct {
	HandoffPath string
	RestorePath string

	handle Handler
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	lns    []net.Listener
}

func NewSupervisor(ctx context.Context, handoffPath, restorePath string, handle Handler) (*Supervisor, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &Supervisor{HandoffPath: handoffPath, RestorePath: restorePath, handle: handle, ctx: ctx, cancel: cancel}

	// libcontainer connects with SOCK_STREAM, CRIU with SOCK_SEQPACKET.
	for _, l := range []struct{ path, network string }{
		{handoffPath, "unix"},
		{restorePath, "unixpacket"},
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
		var once sync.Once
		allow := func() {
			once.Do(func() {
				// ENOENT: the task was killed while its call was held.
				err := libseccomp.NotifRespond(sfd, &libseccomp.ScmpNotifResp{ID: req.ID, Flags: libseccomp.NotifRespFlagContinue})
				if err != nil && !errors.Is(err, unix.ENOENT) {
					log.G(s.ctx).WithError(err).Warn("seccomp notify respond")
				}
			})
		}
		if t, ok := readTrigger(sfd, req); ok {
			s.handle(s.ctx, t, allow)
		}
		allow()
	}
}

func readTrigger(fd libseccomp.ScmpFd, req *libseccomp.ScmpNotifReq) (Trigger, bool) {
	name, err := req.Data.Syscall.GetName()
	if err != nil {
		return Trigger{}, false
	}
	t := Trigger{Pid: int(req.Pid), Syscall: name}
	mem, err := os.Open(fmt.Sprintf("/proc/%d/mem", req.Pid))
	if err != nil {
		return t, true
	}
	defer mem.Close()

	// execve(path, argv, envp); execveat(dirfd, path, argv, envp, flags)
	pathArg, argvArg := req.Data.Args[0], req.Data.Args[1]
	if name == "execveat" {
		pathArg, argvArg = req.Data.Args[1], req.Data.Args[2]
	}
	t.Path = readString(mem, pathArg)
	for i := uint64(0); i < 32 && argvArg != 0; i++ {
		var ptr [8]byte
		if _, err := mem.ReadAt(ptr[:], int64(argvArg+i*8)); err != nil {
			break
		}
		p := binary.LittleEndian.Uint64(ptr[:])
		if p == 0 {
			break
		}
		t.Argv = append(t.Argv, readString(mem, p))
	}
	// The pid may have exited and been reused while we read its memory.
	if libseccomp.NotifIDValid(fd, req.ID) != nil {
		return Trigger{}, false
	}
	return t, true
}

func readString(mem *os.File, addr uint64) string {
	if addr == 0 {
		return ""
	}
	buf := make([]byte, 4096)
	n, _ := mem.ReadAt(buf, int64(addr))
	if i := bytes.IndexByte(buf[:n], 0); i >= 0 {
		n = i
	}
	return string(buf[:n])
}
