//go:build linux

package timetravel

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// errNoExec means the held call did not end in a successful exec.
var errNoExec = errors.New("no exec")

// StopExecArg, as the shim binary's first argument, makes it the tracer
// of stopAfterExec (StopExecMain).
const StopExecArg = "gcr-stop-after-exec"

// stopAfterExec lets the held execve of pid continue (allow) and leaves pid
// stopped (SIGSTOP) just after the new program is loaded, before it runs an
// instruction. A tracer traces pid with PTRACE_O_TRACEEXEC for that one
// event.
//
// Sending SIGSTOP before allowing the call is racy: the notified call waits
// interruptibly, so the stop can interrupt it and the task stops before
// exec instead, with a vfork parent still waiting on it.
//
// The tracer is a process of its own. The container's init is the shim's
// child, and while a thread of its parent's process traces it, the parent's
// every wait sees its ptrace stops: libcontainer's wait for the init's exit
// would take the exec's stop for it.
func stopAfterExec(pid int, allow func(), timeout time.Duration) error {
	r, w, err := os.Pipe()
	if err != nil {
		allow()
		return err
	}
	defer r.Close()
	cmd := exec.Command("/proc/self/exe", StopExecArg, strconv.Itoa(pid), timeout.String())
	cmd.Stdout = w
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: unix.SIGKILL}
	err = cmd.Start()
	w.Close()
	if err != nil {
		allow()
		return fmt.Errorf("start tracer: %w", err)
	}
	// Should the tracer hang, killing it lets go of pid.
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	_ = r.SetReadDeadline(time.Now().Add(timeout + time.Second))
	lines := bufio.NewScanner(r)
	next := func() string {
		if lines.Scan() {
			return lines.Text()
		}
		if lines.Err() != nil {
			return "tracer: " + lines.Err().Error()
		}
		return "tracer exited"
	}
	if s := next(); s != "seized" {
		allow()
		return errors.New(s)
	}
	allow()
	switch s := next(); s {
	case "stopped":
		return nil
	case "noexec":
		return errNoExec
	default:
		return errors.New(s)
	}
}

// StopExecMain is the tracer of stopAfterExec, run with its pid and
// timeout as arguments. It reports on stdout, a line at a time: "seized"
// once it traces pid, then "stopped", "noexec" or an error.
func StopExecMain(args []string) {
	var pid int
	var timeout time.Duration
	var err error
	if len(args) == 2 {
		if pid, err = strconv.Atoi(args[0]); err == nil {
			timeout, err = time.ParseDuration(args[1])
		}
	} else {
		err = fmt.Errorf("usage: %s PID TIMEOUT", StopExecArg)
	}
	if err == nil {
		err = trace(pid, func() { fmt.Println("seized") }, timeout)
	}
	switch {
	case err == nil:
		fmt.Println("stopped")
	case errors.Is(err, errNoExec):
		fmt.Println("noexec")
	default:
		fmt.Println(err)
	}
	os.Exit(0)
}

func trace(pid int, seized func(), timeout time.Duration) error {
	// ptrace requests must come from the thread that attached.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if _, _, e := unix.Syscall6(unix.SYS_PTRACE, unix.PTRACE_SEIZE, uintptr(pid), 0, unix.PTRACE_O_TRACEEXEC, 0, 0); e != 0 {
		return fmt.Errorf("ptrace seize %d: %w", pid, e)
	}
	seized()

	deadline := time.Now().Add(timeout)
	for {
		var ws unix.WaitStatus
		wpid, err := unix.Wait4(pid, &ws, unix.WALL|unix.WNOHANG, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil:
			return fmt.Errorf("wait %d: %w", pid, err)
		case wpid == 0:
			if time.Now().After(deadline) {
				// The exec failed or never finished; let the task go on.
				detach(pid, 0)
				return errNoExec
			}
			time.Sleep(200 * time.Microsecond)
			continue
		case ws.Exited() || ws.Signaled():
			return errNoExec
		case !ws.Stopped():
			continue
		case ws.StopSignal() == unix.SIGTRAP && ws.TrapCause() == unix.PTRACE_EVENT_EXEC:
			// Leave it stopped for the snapshot; the caller continues it.
			// PTRACE_DETACH ignores its signal at an event stop, so queue
			// SIGSTOP first; it is delivered before the first instruction.
			if err := unix.Kill(pid, unix.SIGSTOP); err != nil {
				detach(pid, 0)
				return fmt.Errorf("stop %d: %w", pid, err)
			}
			return detach(pid, 0)
		default:
			// A signal arrived while traced: deliver it and keep waiting.
			sig := ws.StopSignal()
			if ws.StopSignal() == unix.SIGTRAP {
				sig = 0
			}
			if _, _, e := unix.Syscall6(unix.SYS_PTRACE, unix.PTRACE_CONT, uintptr(pid), 0, uintptr(sig), 0, 0); e != 0 {
				return fmt.Errorf("ptrace cont %d: %w", pid, e)
			}
		}
	}
}

func detach(pid int, sig unix.Signal) error {
	if _, _, e := unix.Syscall6(unix.SYS_PTRACE, unix.PTRACE_DETACH, uintptr(pid), 0, uintptr(sig), 0, 0); e != 0 {
		// A running tracee must be stopped before PTRACE_DETACH.
		if e == unix.ESRCH {
			_, _, _ = unix.Syscall6(unix.SYS_PTRACE, unix.PTRACE_INTERRUPT, uintptr(pid), 0, 0, 0, 0)
			var ws unix.WaitStatus
			_, _ = unix.Wait4(pid, &ws, unix.WALL, nil)
			if _, _, e = unix.Syscall6(unix.SYS_PTRACE, unix.PTRACE_DETACH, uintptr(pid), 0, uintptr(sig), 0, 0); e == 0 {
				return nil
			}
		}
		return fmt.Errorf("ptrace detach %d: %w", pid, e)
	}
	return nil
}

// executable reports whether an execve of path by pid can find its file,
// an executable regular file. Shells look a command up by trying execve in
// each PATH directory: the tries that fail are not points, and waiting for
// one to exec would hold the next.
//
// Paths relative to the working directory are not checked.
func executable(pid int, path string) bool {
	if !filepath.IsAbs(path) {
		return path != ""
	}
	root, err := unix.Open(fmt.Sprintf("/proc/%d/root", pid), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return true // let the exec say
	}
	defer unix.Close(root)
	// Resolved as the container would: absolute symlinks within its root.
	fd, err := unix.Openat2(root, strings.TrimLeft(path, "/"), &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTDIR)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return true
	}
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mode&0o111 != 0
}
