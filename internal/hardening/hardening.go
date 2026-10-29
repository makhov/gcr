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
	// DeniedSet also gives unconfined Kubernetes pods the denied set (Denied
	// and Absent), but not the argument rules of gcr's own profile: the
	// calls behind most escapes are closed, while what a pod may use
	// sockets and namespaces for stays as Kubernetes asked. Their seccomp
	// is then not off, which CRI conformance tests expect of Unconfined.
	DeniedSet Level = "denied"
	// Always also hardens unconfined Kubernetes pods with gcr's whole
	// profile. Kubernetes asked for no filter there, so this is the
	// operator overriding it.
	Always Level = "always"
	// Off leaves the workload's seccomp profile as it is.
	Off Level = "off"
)

// NestedUserNamespaces is a runtime handler's policy for user namespaces
// that containers create themselves (Chrome's sandbox, bubblewrap,
// rootless podman), in gcr's own profile; a workload's profile decides for
// itself.
type NestedUserNamespaces string

const (
	// Guarded lets containers create user namespaces, and keeps what root
	// in one reaches small: mounts go through the supervisor's filesystem
	// allowlist, netfilter (nf_tables, the main source of exploits reached
	// this way) is refused to containers without CAP_NET_ADMIN, and the
	// rest of the profile (raw and packet sockets, io_uring, bpf, ...)
	// holds inside too.
	Guarded NestedUserNamespaces = "guarded"
	// Deny refuses CLONE_NEWUSER.
	Deny NestedUserNamespaces = "deny"
)

func (n NestedUserNamespaces) Validate() error {
	switch n {
	case Guarded, Deny:
		return nil
	}
	return fmt.Errorf("NestedUserNamespaces = %q, want guarded or deny: %w", n, errdefs.ErrInvalidArgument)
}

// Options is how Apply hardens a container.
type Options struct {
	Level                Level
	NestedUserNamespaces NestedUserNamespaces
	// Landlocked says the container will be confined with Landlock, which
	// refuses it any mount: the supervisor must not mount for it either.
	Landlocked bool
}

func (l Level) Validate() error {
	switch l {
	case Standard, DeniedSet, Always, Off:
		return nil
	}
	return fmt.Errorf("Hardening = %q, want standard, denied, always or off: %w", l, errdefs.ErrInvalidArgument)
}

// Denied are refused with EPERM whatever the workload's profile allows.
var Denied = []string{
	// io_uring: a second system call interface and a frequent source of
	// kernel bugs.
	"io_uring_setup", "io_uring_enter", "io_uring_register",
	// Kernel programs, profiling and fault handling from user space.
	"bpf", "perf_event_open", "userfaultfd",
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
	// The new mount API; reachable from user namespaces and exploited
	// (CVE-2022-0185). Absent rather than refused: mount(8) from
	// util-linux 2.40 on tries it first and falls back to mount(2), which
	// the supervisor checks, only on ENOSYS; on EPERM it gives up.
	"fsopen", "fsconfig", "fsmount", "fspick", "open_tree", "move_mount", "mount_setattr",
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
func argumentRules(nested NestedUserNamespaces, netAdmin bool) []specs.LinuxSyscall {
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
		// clone3 passes its flags in memory seccomp cannot read; ENOSYS
		// makes libc fall back to clone, whose flags it can.
		{Names: []string{"clone3"}, Action: specs.ActErrno, ErrnoRet: ptr(uint(unix.ENOSYS))},
	}
	switch {
	case nested == Deny:
		// New user namespaces: the doorway to most of the kernel's
		// "unprivileged" attack surface. flags is the first argument of
		// both calls on x86-64 and arm64.
		rules = append(rules, errno([]string{"unshare", "clone"}, masked(0, unix.CLONE_NEWUSER, unix.CLONE_NEWUSER)))
	case !netAdmin:
		// Root in a nested user namespace owns the network namespaces it
		// makes; netfilter is what exploits reach that way.
		rules = append(rules, errno([]string{"socket"}, eq(0, unix.AF_NETLINK), eq(2, unix.NETLINK_NETFILTER)))
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
	// Tunnels made by ioctl on the ipip, gre and sit fallback devices:
	// the netlink policy sees only links made over netlink.
	// (linux/if_tunnel.h: SIOCDEVPRIVATE + 1, 3, 5, 7.)
	const siocAddTunnel, siocChgTunnel, siocAddPRL, siocChgPRL = 0x89f1, 0x89f3, 0x89f5, 0x89f7
	for _, cmd := range []uint64{siocAddTunnel, siocChgTunnel, siocAddPRL, siocChgPRL} {
		rules = append(rules, errno([]string{"ioctl"}, eq(1, cmd)))
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

// Apply hardens spec as o says.
//
// Without a workload profile gcr installs its own: allow everything except
// Denied, Absent and the argument rules (which follow
// o.NestedUserNamespaces). With one, Denied and Absent are merged in and
// win over the profile's own rules for those calls; argument rules are
// not, because combining them with the profile's own argument conditions
// for the same calls cannot be done safely in one filter.
//
// A container that can mount (CAP_SYS_ADMIN, not privileged; in a user
// namespace that capability is scoped to it), or that may make user
// namespaces in which it can, has mount(2) sent to the supervisor, which
// allows only MountFilesystems; with CAP_SYS_ADMIN in a user namespace,
// setns(2) too (see HandleSetns). The caller must then give the filter a
// listener and Handlers.
func Apply(spec *specs.Spec, o Options) Result {
	level := o.Level
	if o.NestedUserNamespaces == "" {
		o.NestedUserNamespaces = Guarded
	}
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
	// Kubernetes' default is Unconfined, and kubelet's seccompDefault
	// makes it RuntimeDefault, which is hardened below like any profile.
	deniedOnly := false
	if sc == nil && ocispec.PodContainer(spec) {
		switch level {
		case Standard:
			return Result{Skipped: "Kubernetes pod with seccomp Unconfined (kubelet seccompDefault, or Hardening = 'denied' or 'always', would harden it)"}
		case DeniedSet:
			deniedOnly = true
		}
	}
	var res Result
	sysAdmin := ocispec.HasCapability(spec, "CAP_SYS_ADMIN")
	// With nested user namespaces, root in one can mount: the allowlist
	// applies there too. Landlock refuses a confined container any mount.
	//
	// Only in a user namespace: without one the supervisor would mount
	// with the init namespace's privilege, which the kernel's own checks
	// and the container's LSM (AppArmor's deny mount, say) would have
	// refused. There, the kernel and the LSM decide, as with runc.
	userns := inUserNamespace(spec)
	supervise := userns && (sysAdmin ||
		(sc == nil && o.NestedUserNamespaces == Guarded && !o.Landlocked))
	if supervise {
		res.Supervised = []string{"mount"}
		if userns && sysAdmin {
			res.Supervised = append(res.Supervised, "setns")
		}
	}
	if sc == nil && deniedOnly {
		sc = &specs.LinuxSeccomp{
			DefaultAction: specs.ActAllow,
			Architectures: nativeArchitectures(),
			Syscalls:      denyRules(),
		}
		spec.Linux.Seccomp = sc
	} else if sc == nil {
		allowPing(spec)
		sc = &specs.LinuxSeccomp{
			DefaultAction: specs.ActAllow,
			Architectures: nativeArchitectures(),
			Syscalls:      append(denyRules(), argumentRules(o.NestedUserNamespaces, ocispec.HasCapability(spec, "CAP_NET_ADMIN"))...),
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
	if sysAdmin && userns && sc.DefaultAction != specs.ActAllow {
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
