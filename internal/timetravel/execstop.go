//go:build linux

package timetravel

import (
	"errors"
	"fmt"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// errNoExec means the held call did not end in a successful exec.
var errNoExec = errors.New("no exec")

// stopAfterExec lets the held execve of pid continue (allow) and leaves pid
// stopped (SIGSTOP) just after the new program is loaded, before it runs an
// instruction. It traces pid with PTRACE_O_TRACEEXEC for that one event.
//
// Sending SIGSTOP before allowing the call is racy: the notified call waits
// interruptibly, so the stop can interrupt it and the task stops before
// exec instead, with a vfork parent still waiting on it.
func stopAfterExec(pid int, allow func(), timeout time.Duration) error {
	// ptrace requests must come from the thread that attached.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if _, _, e := unix.Syscall6(unix.SYS_PTRACE, unix.PTRACE_SEIZE, uintptr(pid), 0, unix.PTRACE_O_TRACEEXEC, 0, 0); e != 0 {
		allow()
		return fmt.Errorf("ptrace seize %d: %w", pid, e)
	}
	allow()

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
