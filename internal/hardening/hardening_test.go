//go:build linux

package hardening

import (
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

func TestApplyWithoutProfile(t *testing.T) {
	s := &specs.Spec{Process: &specs.Process{}}
	if r := Apply(s, Options{Level: Standard, NestedUserNamespaces: Deny, Landlocked: true}); r.Skipped != "" || len(r.Supervised) != 0 {
		t.Fatalf("Apply = %+v", r)
	}
	sc := s.Linux.Seccomp
	if sc.DefaultAction != specs.ActAllow {
		t.Fatalf("default action %s", sc.DefaultAction)
	}
	if !slices.Equal(sc.Syscalls[0].Names, Denied) || sc.Syscalls[0].Action != specs.ActErrno {
		t.Fatalf("first rule %+v", sc.Syscalls[0])
	}
	if r := sc.Syscalls[1]; !slices.Equal(r.Names, Absent) || *r.ErrnoRet != uint(unix.ENOSYS) {
		t.Fatalf("second rule %+v", r)
	}
	var userns, clone3, raw bool
	for _, r := range sc.Syscalls[2:] {
		switch {
		case slices.Contains(r.Names, "unshare") && len(r.Args) == 1 && r.Args[0].Op == specs.OpMaskedEqual:
			userns = true
		case slices.Equal(r.Names, []string{"clone3"}):
			clone3 = true
		case slices.Equal(r.Names, []string{"socket"}) && len(r.Args) == 2:
			raw = true
		}
	}
	if !userns || !clone3 || !raw {
		t.Fatalf("argument rules missing: userns=%v clone3=%v raw=%v", userns, clone3, raw)
	}
}

func TestApplyMergesIntoProfile(t *testing.T) {
	s := &specs.Spec{Process: &specs.Process{}, Linux: &specs.Linux{Seccomp: &specs.LinuxSeccomp{
		DefaultAction: specs.ActErrno,
		Syscalls: []specs.LinuxSyscall{
			{Names: []string{"read", "bpf", "io_uring_setup"}, Action: specs.ActAllow},
			{Names: []string{"keyctl"}, Action: specs.ActAllow},
			{Names: []string{"clone"}, Action: specs.ActAllow, Args: []specs.LinuxSeccompArg{{Index: 0, Value: 1, Op: specs.OpMaskedEqual}}},
		},
	}}}
	Apply(s, Options{Level: Standard})
	rules := s.Linux.Seccomp.Syscalls
	if len(rules) != 4 {
		t.Fatalf("rules %+v", rules)
	}
	if !slices.Equal(rules[0].Names, []string{"read"}) {
		t.Fatalf("denied calls left in the profile's allow rule: %v", rules[0].Names)
	}
	if !slices.Equal(rules[1].Names, []string{"clone"}) || len(rules[1].Args) != 1 {
		t.Fatalf("profile's argument rule changed: %+v", rules[1])
	}
	if r := rules[2]; !slices.Equal(r.Names, Denied) || r.Action != specs.ActErrno {
		t.Fatalf("denied rule %+v", r)
	}
	if r := rules[3]; !slices.Equal(r.Names, Absent) || *r.ErrnoRet != uint(unix.ENOSYS) {
		t.Fatalf("absent rule %+v", r)
	}
}

func TestApplySkips(t *testing.T) {
	priv := &specs.Spec{
		Process: &specs.Process{Capabilities: &specs.LinuxCapabilities{Bounding: []string{"CAP_SYS_ADMIN"}}},
		Linux:   &specs.Linux{Resources: &specs.LinuxResources{Devices: []specs.LinuxDeviceCgroup{{Allow: true, Access: "rwm"}}}},
	}
	if r := Apply(priv, Options{Level: Standard}); r.Skipped == "" || priv.Linux.Seccomp != nil {
		t.Fatal("privileged container hardened")
	}
	unconfinedPod := func() *specs.Spec {
		return &specs.Spec{Process: &specs.Process{}, Annotations: map[string]string{"io.kubernetes.cri.container-type": "container"}}
	}
	if s := unconfinedPod(); Apply(s, Options{Level: Standard}).Skipped == "" || s.Linux.Seccomp != nil {
		t.Fatal("standard: unconfined Kubernetes pod got a filter")
	}
	if s := unconfinedPod(); Apply(s, Options{Level: Always}).Skipped != "" || s.Linux.Seccomp == nil {
		t.Fatal("always: unconfined Kubernetes pod not hardened")
	}
	off := &specs.Spec{Process: &specs.Process{}}
	if r := Apply(off, Options{Level: Off}); r.Skipped == "" || off.Linux != nil {
		t.Fatal("Hardening = off applied a profile")
	}
	if err := Level("paranoid").Validate(); err == nil {
		t.Fatal("bad level accepted")
	}
}

func TestApplySupervisesMountWithSysAdmin(t *testing.T) {
	// CAP_SYS_ADMIN without access to all devices: not privileged, can mount.
	s := &specs.Spec{
		Process: &specs.Process{Capabilities: &specs.LinuxCapabilities{Bounding: []string{"CAP_SYS_ADMIN"}}},
		Linux:   &specs.Linux{MaskedPaths: []string{"/proc/kcore"}},
	}
	r := Apply(s, Options{Level: Standard})
	if r.Skipped != "" || !slices.Equal(r.Supervised, []string{"mount"}) {
		t.Fatalf("Apply = %+v", r)
	}
	last := s.Linux.Seccomp.Syscalls[len(s.Linux.Seccomp.Syscalls)-1]
	if !slices.Equal(last.Names, []string{"mount"}) || last.Action != specs.ActNotify {
		t.Fatalf("mount rule %+v", last)
	}

	// Merged into a profile that allows mount: the notify rule replaces it.
	s = &specs.Spec{
		Process: &specs.Process{Capabilities: &specs.LinuxCapabilities{Bounding: []string{"CAP_SYS_ADMIN"}}},
		Linux: &specs.Linux{MaskedPaths: []string{"/proc/kcore"}, Seccomp: &specs.LinuxSeccomp{
			DefaultAction: specs.ActErrno,
			Syscalls:      []specs.LinuxSyscall{{Names: []string{"mount", "read"}, Action: specs.ActAllow}},
		}},
	}
	Apply(s, Options{Level: Standard})
	for _, rule := range s.Linux.Seccomp.Syscalls {
		if slices.Contains(rule.Names, "mount") && rule.Action != specs.ActNotify {
			t.Fatalf("mount still allowed by %+v", rule)
		}
	}
}

func TestApplySupervisesSetnsInUserNamespace(t *testing.T) {
	s := &specs.Spec{
		Process: &specs.Process{Capabilities: &specs.LinuxCapabilities{Bounding: []string{"CAP_SYS_ADMIN"}}},
		Linux:   &specs.Linux{Namespaces: []specs.LinuxNamespace{{Type: specs.UserNamespace}}},
	}
	r := Apply(s, Options{Level: Standard})
	if !slices.Equal(r.Supervised, []string{"mount", "setns"}) {
		t.Fatalf("Apply = %+v", r)
	}

	// A default-deny profile gets pivot_root for runtimes inside.
	s.Linux.Seccomp = &specs.LinuxSeccomp{DefaultAction: specs.ActErrno}
	Apply(s, Options{Level: Standard})
	last := s.Linux.Seccomp.Syscalls[len(s.Linux.Seccomp.Syscalls)-1]
	if !slices.Equal(last.Names, []string{"pivot_root"}) || last.Action != specs.ActAllow {
		t.Fatalf("pivot_root rule %+v", last)
	}
	for _, name := range r.Supervised {
		if Handlers[name] == nil {
			t.Errorf("no handler for %s", name)
		}
	}
}

func TestCheckMount(t *testing.T) {
	for _, tc := range []struct {
		fstype string
		flags  uintptr
		ok     bool
	}{
		{"tmpfs", 0, true},
		{"overlay", 0, true},
		{"cgroup2", 0, true},
		{"ramfs", 0, false},
		{"fuse", 0, false},
		{"ext4", 0, false},
		{"", unix.MS_BIND | unix.MS_REC, true},
		{"", unix.MS_REMOUNT | unix.MS_RDONLY, true},
		{"", unix.MS_PRIVATE, true},
	} {
		if got := checkMount(tc.fstype, tc.flags) == 0; got != tc.ok {
			t.Errorf("checkMount(%q, %#x) allowed=%v, want %v", tc.fstype, tc.flags, got, tc.ok)
		}
	}
}

// rule finds a rule for name whose first argument condition is on index
// with value.
func rule(rules []specs.LinuxSyscall, name string, index uint, value uint64) *specs.LinuxSyscall {
	for i, r := range rules {
		if slices.Contains(r.Names, name) && len(r.Args) > 0 && r.Args[0].Index == index && r.Args[0].Value == value {
			return &rules[i]
		}
	}
	return nil
}

func TestNestedUserNamespacesGuarded(t *testing.T) {
	plain := func(caps ...string) *specs.Spec {
		return &specs.Spec{Process: &specs.Process{Capabilities: &specs.LinuxCapabilities{Bounding: caps}}}
	}
	s := plain()
	r := Apply(s, Options{Level: Standard})
	rules := s.Linux.Seccomp.Syscalls
	if rule(rules, "unshare", 0, unix.CLONE_NEWUSER) != nil {
		t.Fatal("CLONE_NEWUSER refused in guarded mode")
	}
	if rule(rules, "socket", 0, unix.AF_NETLINK) == nil {
		t.Fatal("netfilter not refused")
	}
	if !slices.Equal(r.Supervised, []string{"mount"}) {
		t.Fatalf("mount not supervised: %+v", r)
	}

	// Landlock refuses a confined container any mount; no supervision.
	if r := Apply(plain(), Options{Level: Standard, Landlocked: true}); len(r.Supervised) != 0 {
		t.Fatalf("landlocked container supervised: %+v", r)
	}
	// CAP_NET_ADMIN: netfilter is the container's to use already.
	s = plain("CAP_NET_ADMIN")
	Apply(s, Options{Level: Standard})
	if rule(s.Linux.Seccomp.Syscalls, "socket", 0, unix.AF_NETLINK) != nil {
		t.Fatal("netfilter refused to a container with CAP_NET_ADMIN")
	}
	// Deny refuses CLONE_NEWUSER.
	s = plain()
	Apply(s, Options{Level: Standard, NestedUserNamespaces: Deny})
	if rule(s.Linux.Seccomp.Syscalls, "unshare", 0, unix.CLONE_NEWUSER) == nil {
		t.Fatal("CLONE_NEWUSER allowed with NestedUserNamespaces = deny")
	}
}
