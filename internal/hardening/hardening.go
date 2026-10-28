//go:build linux

// Package hardening narrows the kernel surface a container can reach,
// whatever seccomp profile the workload brings: the "always denied" set of
// the design (calls behind most recent container escapes and kernel
// exploits reachable from containers).
package hardening

import (
	"fmt"
	"slices"

	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/ocispec"
	"github.com/makhov/gcr/internal/supervisor"
)

// Level is a runtime handler's hardening policy.
type Level string

const (
	// Standard hardens every container except privileged ones and
	// Kubernetes pods that run explicitly unconfined (Kubernetes' default
	// seccomp profile is Unconfined, and a runtime cannot tell the default
	// from an explicit choice).
	Standard Level = "standard"
	// Always also hardens unconfined Kubernetes pods. Kubernetes asked for
	// no filter there, so this is the operator overriding it.
	Always Level = "always"
	// Off leaves the workload's seccomp profile as it is.
	Off Level = "off"
)

// containerTypeAnnotation is set by CRI on every pod container.
const containerTypeAnnotation = "io.kubernetes.cri.container-type"

func (l Level) Validate() error {
	switch l {
	case Standard, Always, Off:
		return nil
	}
	return fmt.Errorf("Hardening = %q, want standard, always or off: %w", l, errdefs.ErrInvalidArgument)
}

// Denied are refused with EPERM whatever the workload's profile allows.
var Denied = []string{
	// io_uring: a second system call interface and a frequent source of
	// kernel bugs.
	"io_uring_setup", "io_uring_enter", "io_uring_register",
	// Kernel programs, profiling and fault handling from user space.
	"bpf", "perf_event_open", "userfaultfd",
	// The new mount API; reachable from user namespaces and exploited
	// (CVE-2022-0185). Mounts for Docker-in-Docker go through the
	// supervisor later.
	"fsopen", "fsconfig", "fsmount", "fspick", "open_tree", "move_mount", "mount_setattr",
	// Kernel code loading and replacement.
	"init_module", "finit_module", "delete_module", "kexec_load", "kexec_file_load",
	// File handles bypass path checks ("shocker").
	"open_by_handle_at",
}

// Absent are refused with ENOSYS, as if the kernel lacked them: callers
// treat that as "no such feature" and carry on (runc only skips joining a
// session keyring on ENOSYS).
var Absent = []string{
	// The kernel keyring is not namespaced.
	"add_key", "keyctl", "request_key",
}

const eperm = uint(unix.EPERM)

// denyRules refuse Denied and Absent.
func denyRules() []specs.LinuxSyscall {
	return []specs.LinuxSyscall{
		{Names: slices.Clone(Denied), Action: specs.ActErrno, ErrnoRet: ptr(eperm)},
		{Names: slices.Clone(Absent), Action: specs.ActErrno, ErrnoRet: ptr(uint(unix.ENOSYS))},
	}
}

// argumentRules deny by argument; they only go into gcr's own profile
// (see Apply).
func argumentRules() []specs.LinuxSyscall {
	errno := func(names []string, args ...specs.LinuxSeccompArg) specs.LinuxSyscall {
		return specs.LinuxSyscall{Names: names, Action: specs.ActErrno, ErrnoRet: ptr(eperm), Args: args}
	}
	masked := func(index uint, mask, value uint64) specs.LinuxSeccompArg {
		return specs.LinuxSeccompArg{Index: index, Value: mask, ValueTwo: value, Op: specs.OpMaskedEqual}
	}
	eq := func(index uint, value uint64) specs.LinuxSeccompArg {
		return specs.LinuxSeccompArg{Index: index, Value: value, Op: specs.OpEqualTo}
	}
	rules := []specs.LinuxSyscall{
		// New user namespaces: the doorway to most of the kernel's
		// "unprivileged" attack surface. flags is the first argument of
		// both calls on x86-64 and arm64.
		errno([]string{"unshare", "clone"}, masked(0, unix.CLONE_NEWUSER, unix.CLONE_NEWUSER)),
		// clone3 passes its flags in memory seccomp cannot read; ENOSYS
		// makes libc fall back to clone.
		{Names: []string{"clone3"}, Action: specs.ActErrno, ErrnoRet: ptr(uint(unix.ENOSYS))},
	}
	// Socket families with little legitimate use in containers and a
	// record of exploits.
	for _, family := range []uint64{unix.AF_PACKET, unix.AF_VSOCK, unix.AF_KEY, unix.AF_ALG} {
		rules = append(rules, errno([]string{"socket"}, eq(0, family)))
	}
	// Raw IP sockets (ping uses ICMP datagram sockets instead).
	for _, family := range []uint64{unix.AF_INET, unix.AF_INET6} {
		rules = append(rules, errno([]string{"socket"}, eq(0, family), masked(1, 0xf, unix.SOCK_RAW)))
	}
	return rules
}

// Result says what Apply did.
type Result struct {
	// Skipped says why spec was left alone, "" if it was hardened.
	Skipped string
	// Supervised lists calls the filter now hands to the container's
	// supervisor (mount, for containers that can mount).
	Supervised []string
}

// Apply hardens spec at level.
//
// Without a workload profile gcr installs its own: allow everything except
// Denied, Absent and the argument rules. With one, those two are merged in and wins
// over the profile's own rules for those calls; argument rules are not,
// because combining them with the profile's own argument conditions for the
// same calls cannot be done safely in one filter.
//
// A container that can mount (CAP_SYS_ADMIN, not privileged; in a user
// namespace that capability is scoped to it) has mount(2) sent to the
// supervisor, which allows only MountFilesystems, and in a user namespace
// setns(2) too (see HandleSetns). The caller must then give the filter a
// listener and Handlers.
func Apply(spec *specs.Spec, level Level) Result {
	if level == Off {
		return Result{Skipped: "Hardening = off"}
	}
	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}
	if ocispec.Privileged(spec) {
		return Result{Skipped: "privileged container"}
	}
	sc := spec.Linux.Seccomp
	if sc == nil && level == Standard && spec.Annotations[containerTypeAnnotation] != "" {
		return Result{Skipped: "Kubernetes pod with seccomp Unconfined"}
	}
	var res Result
	supervise := ocispec.HasCapability(spec, "CAP_SYS_ADMIN")
	userns := inUserNamespace(spec)
	if supervise {
		res.Supervised = []string{"mount"}
		if userns {
			res.Supervised = append(res.Supervised, "setns")
		}
	}
	if sc == nil {
		allowPing(spec)
		sc = &specs.LinuxSeccomp{
			DefaultAction: specs.ActAllow,
			Architectures: nativeArchitectures(),
			Syscalls:      append(denyRules(), argumentRules()...),
		}
		spec.Linux.Seccomp = sc
	} else {
		sc.Syscalls = without(without(sc.Syscalls, Denied), Absent)
		sc.Syscalls = append(sc.Syscalls, denyRules()...)
	}
	if supervise {
		sc.Syscalls = without(sc.Syscalls, res.Supervised)
		sc.Syscalls = append(sc.Syscalls, specs.LinuxSyscall{Names: slices.Clone(res.Supervised), Action: specs.ActNotify})
	}
	if supervise && userns && sc.DefaultAction != specs.ActAllow {
		// Container runtimes inside need pivot_root, which default
		// profiles leave out. It only changes the caller's own mount
		// namespace, which a user namespace's CAP_SYS_ADMIN already
		// lets it rearrange through the supervised mount.
		sc.Syscalls = without(sc.Syscalls, nestedRuntime)
		sc.Syscalls = append(sc.Syscalls, specs.LinuxSyscall{Names: slices.Clone(nestedRuntime), Action: specs.ActAllow})
	}
	return res
}

// nestedRuntime are calls a container runtime inside a container needs
// beyond mount and setns.
var nestedRuntime = []string{"pivot_root"}

// Handlers are the supervisor handlers for the calls Apply may supervise.
var Handlers = map[string]supervisor.Handler{
	"mount": HandleMount,
	"setns": HandleSetns,
}

func inUserNamespace(spec *specs.Spec) bool {
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.UserNamespace {
			return true
		}
	}
	return false
}

// without drops names from rules, removing rules left empty.
func without(rules []specs.LinuxSyscall, names []string) []specs.LinuxSyscall {
	var out []specs.LinuxSyscall
	for _, r := range rules {
		r.Names = slices.DeleteFunc(slices.Clone(r.Names), func(n string) bool {
			return slices.Contains(names, n)
		})
		if len(r.Names) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// allowPing opens ICMP datagram sockets (ping without raw sockets, which
// gcr's profile denies) in a network namespace the container creates.
// Pods' namespaces get this from containerd, or from userns.OpenNetwork.
func allowPing(spec *specs.Spec) {
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type != specs.NetworkNamespace || ns.Path != "" {
			continue
		}
		if spec.Linux.Sysctl == nil {
			spec.Linux.Sysctl = map[string]string{}
		}
		if _, set := spec.Linux.Sysctl["net.ipv4.ping_group_range"]; !set {
			// In a user namespace the range may only name mapped groups.
			last := uint32(2147483647)
			if m := spec.Linux.GIDMappings; len(m) > 0 {
				last = 0
				for _, g := range m {
					last = max(last, g.ContainerID+g.Size-1)
				}
			}
			spec.Linux.Sysctl["net.ipv4.ping_group_range"] = fmt.Sprintf("0 %d", last)
		}
	}
}

// nativeArchitectures covers the host and its 32-bit compat ABI, so a
// container cannot dodge the filter through the other system call table.
func nativeArchitectures() []specs.Arch {
	var u unix.Utsname
	_ = unix.Uname(&u)
	switch unix.ByteSliceToString(u.Machine[:]) {
	case "x86_64":
		return []specs.Arch{specs.ArchX86_64, specs.ArchX86, specs.ArchX32}
	case "aarch64":
		return []specs.Arch{specs.ArchAARCH64, specs.ArchARM}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
