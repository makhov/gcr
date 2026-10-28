//go:build linux

package hardening

import (
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestApplyWithoutProfile(t *testing.T) {
	s := &specs.Spec{Process: &specs.Process{}}
	if why := Apply(s, Standard); why != "" {
		t.Fatalf("not applied: %s", why)
	}
	sc := s.Linux.Seccomp
	if sc.DefaultAction != specs.ActAllow {
		t.Fatalf("default action %s", sc.DefaultAction)
	}
	if !slices.Equal(sc.Syscalls[0].Names, Denied) || sc.Syscalls[0].Action != specs.ActErrno {
		t.Fatalf("first rule %+v", sc.Syscalls[0])
	}
	var userns, clone3, raw bool
	for _, r := range sc.Syscalls[1:] {
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
	Apply(s, Standard)
	rules := s.Linux.Seccomp.Syscalls
	if len(rules) != 3 {
		t.Fatalf("rules %+v", rules)
	}
	if !slices.Equal(rules[0].Names, []string{"read"}) {
		t.Fatalf("denied calls left in the profile's allow rule: %v", rules[0].Names)
	}
	if !slices.Equal(rules[1].Names, []string{"clone"}) || len(rules[1].Args) != 1 {
		t.Fatalf("profile's argument rule changed: %+v", rules[1])
	}
	if last := rules[2]; !slices.Equal(last.Names, Denied) || last.Action != specs.ActErrno {
		t.Fatalf("denied rule %+v", last)
	}
}

func TestApplySkips(t *testing.T) {
	priv := &specs.Spec{Process: &specs.Process{Capabilities: &specs.LinuxCapabilities{Bounding: []string{"CAP_SYS_ADMIN"}}}}
	if why := Apply(priv, Standard); why == "" || priv.Linux.Seccomp != nil {
		t.Fatal("privileged container hardened")
	}
	unconfinedPod := func() *specs.Spec {
		return &specs.Spec{Process: &specs.Process{}, Annotations: map[string]string{containerTypeAnnotation: "container"}}
	}
	if s := unconfinedPod(); Apply(s, Standard) == "" || s.Linux.Seccomp != nil {
		t.Fatal("standard: unconfined Kubernetes pod got a filter")
	}
	if s := unconfinedPod(); Apply(s, Always) != "" || s.Linux.Seccomp == nil {
		t.Fatal("always: unconfined Kubernetes pod not hardened")
	}
	off := &specs.Spec{Process: &specs.Process{}}
	if why := Apply(off, Off); why == "" || off.Linux != nil {
		t.Fatal("Hardening = off applied a profile")
	}
	if err := Level("paranoid").Validate(); err == nil {
		t.Fatal("bad level accepted")
	}
}
