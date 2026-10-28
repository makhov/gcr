//go:build linux

package landlock

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/opencontainers/runc/libcontainer/configs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

func TestDecide(t *testing.T) {
	if ABI() < 1 {
		t.Skip("kernel without Landlock")
	}
	plain := func() *specs.Spec {
		return &specs.Spec{Process: &specs.Process{}, Linux: &specs.Linux{},
			Annotations: map[string]string{Annotation: "forged"}}
	}
	s := plain()
	if why := Decide(s, Standard); why != "" || s.Annotations[Annotation] != confineToRoot {
		t.Fatalf("plain container: %q, annotations %v", why, s.Annotations)
	}
	s = plain()
	if why := Decide(s, Off); why == "" || s.Annotations[Annotation] != "" {
		t.Fatalf("Landlock = off: %q, annotations %v (the workload's value must go)", why, s.Annotations)
	}
	s = plain()
	s.Process.Capabilities = &specs.LinuxCapabilities{Bounding: []string{"CAP_SYS_ADMIN"}}
	if why := Decide(s, Standard); why == "" || s.Annotations[Annotation] != "" {
		t.Fatalf("container that can mount: %q", why)
	}
	s = plain()
	s.Linux.Namespaces = []specs.LinuxNamespace{{Type: specs.PIDNamespace, Path: "/proc/42/ns/pid"}}
	if why := Decide(s, Standard); why == "" || s.Annotations[Annotation] != "" {
		t.Fatalf("debug container: %q", why)
	}
	s = plain()
	s.Linux.Namespaces = []specs.LinuxNamespace{{Type: specs.PIDNamespace}, {Type: specs.NetworkNamespace, Path: "/proc/42/ns/net"}}
	if why := Decide(s, Standard); why != "" {
		t.Fatalf("pod container with its own PID namespace left out: %q", why)
	}
}

func TestHandledFS(t *testing.T) {
	if HandledFS(0) != 0 {
		t.Fatal("rights without Landlock")
	}
	if got := HandledFS(1); got != 0x1fff {
		t.Fatalf("ABI 1: %#x", got)
	}
	if got := HandledFS(5); got&unix.LANDLOCK_ACCESS_FS_IOCTL_DEV == 0 || got&unix.LANDLOCK_ACCESS_FS_REFER == 0 {
		t.Fatalf("ABI 5: %#x", got)
	}
}

// TestRestrict confines a child process to a directory, as InitHook does
// with the container's root, and checks what it can still open.
func TestRestrict(t *testing.T) {
	if dir := os.Getenv("LANDLOCK_TEST_ROOT"); dir != "" {
		restrictedChild(dir)
		return
	}
	if ABI() < 1 {
		t.Skip("kernel without Landlock")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inside"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kept"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRestrict$")
	cmd.Env = append(os.Environ(), "LANDLOCK_TEST_ROOT="+dir, "LANDLOCK_TEST_OUTSIDE="+outside)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restricted child: %v\n%s", err, out)
	}
}

func restrictedChild(dir string) {
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", a...)
		os.Exit(1)
	}
	kept, err := os.OpenFile(os.Getenv("LANDLOCK_TEST_OUTSIDE"), unix.O_PATH, 0)
	if err != nil {
		fail("open kept file")
	}
	root, err := os.OpenFile(dir, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		fail("open root")
	}
	// The container's init still holds CAP_SYS_ADMIN here; a test process
	// needs no_new_privs. The restriction is the calling thread's.
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		fail("no_new_privs: %v", err)
	}
	handled := HandledFS(ABI())
	if err := restrict(handled, []rule{{root, handled}, {kept, handled & fileRights}}); err != nil {
		fail("%v", err)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "inside")); err != nil {
		fail("inside the root: %v", err)
	}
	if _, err := os.ReadFile("/etc/hostname"); !errors.Is(err, unix.EACCES) {
		fail("outside the root: got %v, want EACCES", err)
	}
	// A kept file opens through /proc, the way libcontainer reopens the
	// exec fifo; a directory fd from before the restriction leads nowhere.
	if f, err := os.OpenFile("/proc/self/fd/"+strconv.Itoa(int(kept.Fd())), os.O_WRONLY, 0); err != nil {
		fail("kept file: %v", err)
	} else {
		f.Close()
	}
	etc, err := os.Open("/etc")
	if err == nil {
		etc.Close()
		fail("opened /etc")
	}
	os.Exit(0)
}

func TestInitHookSkipsUnmarked(t *testing.T) {
	if err := InitHook(&configs.Config{Labels: []string{"bundle=/x"}}, nil); err != nil {
		t.Fatal(err)
	}
}
