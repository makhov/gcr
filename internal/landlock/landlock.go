//go:build linux

// Package landlock confines a container's processes to its root
// filesystem with Landlock: whatever they reach outside it (a leaked host
// file descriptor or working directory, a /proc/<pid>/root link to another
// mount namespace), they cannot open. Inside, Landlock adds nothing; the
// container's own mounts and permissions decide.
//
// The rule is applied in the container's init, through libcontainer's
// InitHook (gcr's runc fork), once the root filesystem is final and before
// capabilities drop, so it needs no no_new_privs and setuid binaries keep
// working. It is inherited by everything the init starts. Processes
// started in a running container (exec) are left out: Landlock lets a
// process trace and inspect (/proc/<pid>/fd, environ, root) only processes
// in its own or a nested domain, and an exec gets a domain of its own, so
// debuggers and tools like jcmd would stop working on the container.
package landlock

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"unsafe"

	"github.com/containerd/errdefs"
	"github.com/cyphar/filepath-securejoin/pathrs-lite/procfs"
	"github.com/opencontainers/runc/libcontainer/configs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/ocispec"
)

// Level is a runtime handler's Landlock policy.
type Level string

const (
	// Standard confines every container that cannot mount: Landlock forbids
	// mount changes, which containers with CAP_SYS_ADMIN (Docker-in-Docker,
	// nested runtimes) need.
	Standard Level = "standard"
	// Off confines no container.
	Off Level = "off"
)

func (l Level) Validate() error {
	switch l {
	case Standard, Off:
		return nil
	}
	return fmt.Errorf("Landlock = %q, want standard or off: %w", l, errdefs.ErrInvalidArgument)
}

// Annotation marks a container to confine. gcr sets it, replacing any value
// the workload brought; the init hook reads it from the container's config.
const Annotation = "gcr.dev/landlock"

const confineToRoot = "root"

// Decide marks spec for confinement at level, or says why not.
func Decide(spec *specs.Spec, level Level) (why string) {
	delete(spec.Annotations, Annotation)
	switch {
	case level == Off:
		return "Landlock = off"
	case ocispec.Privileged(spec):
		return "privileged container"
	case ocispec.HasCapability(spec, "CAP_SYS_ADMIN"):
		return "container can mount"
	case ABI() < 1:
		return "kernel without Landlock"
	}
	if spec.Annotations == nil {
		spec.Annotations = map[string]string{}
	}
	spec.Annotations[Annotation] = confineToRoot
	return ""
}

// ABI is the kernel's Landlock ABI version, 0 without Landlock.
func ABI() int {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(v)
}

// fileRights are the access rights that apply to files, not directories.
const fileRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV

// HandledFS are the filesystem access rights the kernel's ABI knows.
func HandledFS(abi int) uint64 {
	if abi < 1 {
		return 0
	}
	rights := uint64(unix.LANDLOCK_ACCESS_FS_MAKE_SYM<<1 - 1) // ABI 1
	if abi >= 2 {
		rights |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		rights |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		rights |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return rights
}

// InitHook confines the container's init, if Decide marked it, to its root
// filesystem; keep are host files the init still opens. It is
// libcontainer's InitHook.
func InitHook(c *configs.Config, keep []*os.File) error {
	if !slices.Contains(c.Labels, Annotation+"="+confineToRoot) {
		return nil
	}
	handled := HandledFS(ABI())
	if handled == 0 {
		return fmt.Errorf("landlock: kernel without Landlock")
	}
	root, err := os.OpenFile("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	defer root.Close()
	proc, err := privateProc()
	if err != nil {
		// libcontainer would reach /proc through a new mount each time,
		// which no rule can name.
		logrus.WithError(err).Warn("landlock: not confining the container")
		return nil
	}
	defer proc.Close()
	rules := []rule{{root, handled}, {proc, handled}}
	for _, f := range keep {
		if f != nil {
			rules = append(rules, rule{f, handled & fileRights})
		}
	}
	return restrict(handled, rules)
}

// privateProc opens the root of the procfs instance libcontainer goes
// through for the rest of the init (pathrs-lite's: a private mount, cached
// for the process, outside the container's root).
func privateProc() (*os.File, error) {
	open := func() (*os.File, error) {
		h, err := procfs.OpenProcRoot()
		if err != nil {
			return nil, err
		}
		defer h.Close()
		return h.OpenRoot(".")
	}
	a, err := open()
	if err != nil {
		return nil, err
	}
	b, err := open()
	if err != nil {
		a.Close()
		return nil, err
	}
	defer b.Close()
	var sa, sb unix.Stat_t
	if unix.Fstat(int(a.Fd()), &sa) != nil || unix.Fstat(int(b.Fd()), &sb) != nil || sa.Dev != sb.Dev {
		a.Close()
		return nil, fmt.Errorf("procfs handle not cached")
	}
	return a, nil
}

// CRIUOption is the CRIU config line that confines a restored container
// again: the kernel cannot report a process's Landlock domain, so CRIU
// cannot dump it. ok is false when the installed CRIU lacks --landlock-fs
// (gcr's CRIU fork has it).
func CRIUOption() (opt string, ok bool) {
	if !criuHasLandlock() {
		return "", false
	}
	return fmt.Sprintf("landlock-fs %#x", HandledFS(ABI())), true
}

var criuHasLandlock = sync.OnceValue(func() bool {
	out, _ := exec.Command("criu", "--help").CombinedOutput()
	return bytes.Contains(out, []byte("--landlock-fs"))
})

type rule struct {
	f      *os.File
	access uint64
}

// restrict confines the calling thread, and what it starts, to rules.
func restrict(handled uint64, rules []rule) error {
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock: create ruleset: %w", errno)
	}
	defer unix.Close(int(fd))
	for _, r := range rules {
		pb := unix.LandlockPathBeneathAttr{Allowed_access: r.access, Parent_fd: int32(r.f.Fd())}
		if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&pb)), 0, 0, 0); errno != 0 {
			return fmt.Errorf("landlock: add rule for %s: %w", r.f.Name(), errno)
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock: restrict: %w", errno)
	}
	return nil
}
