//go:build linux

package userns

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func podSpec(annotations map[string]string, ns ...specs.LinuxNamespace) *specs.Spec {
	return &specs.Spec{
		Annotations: annotations,
		Process:     &specs.Process{Capabilities: &specs.LinuxCapabilities{Bounding: []string{"CAP_CHOWN"}}},
		Linux:       &specs.Linux{Namespaces: ns},
	}
}

// own namespaces of the kinds Wanted checks, so the spec does not share
// the host's.
var ownNamespaces = []specs.LinuxNamespace{
	{Type: specs.NetworkNamespace}, {Type: specs.PIDNamespace}, {Type: specs.IPCNamespace}, {Type: specs.MountNamespace},
}

func TestWanted(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *specs.Spec
		want bool
	}{
		{"plain container", podSpec(nil, ownNamespaces...), true},
		{"opt out", podSpec(map[string]string{AnnotationHostUsers: "true"}, ownNamespaces...), false},
		{"kubernetes pod follows hostUsers", podSpec(map[string]string{"io.kubernetes.cri.container-type": "container"}, ownNamespaces...), false},
		{"kubernetes pod opted in", podSpec(map[string]string{"io.kubernetes.cri.container-type": "container", AnnotationHostUsers: "false"}, ownNamespaces...), true},
		{"CRI-O pod follows hostUsers", podSpec(map[string]string{"io.kubernetes.cri-o.ContainerType": "container"}, ownNamespaces...), false},
		{"own user namespace", podSpec(nil, append(slices.Clone(ownNamespaces), specs.LinuxNamespace{Type: specs.UserNamespace})...), false},
		{"host network (no entry)", podSpec(nil, specs.LinuxNamespace{Type: specs.PIDNamespace}, specs.LinuxNamespace{Type: specs.IPCNamespace}), false},
		{"host network (path)", podSpec(nil, specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: "/proc/1/ns/net"},
			specs.LinuxNamespace{Type: specs.PIDNamespace}, specs.LinuxNamespace{Type: specs.IPCNamespace}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why, err := Decide(tc.spec, Auto)
			if err != nil || got != tc.want {
				t.Fatalf("Wanted = %v (%s), want %v", got, why, tc.want)
			}
		})
	}

	t.Run("privileged", func(t *testing.T) {
		s := podSpec(nil, ownNamespaces...)
		s.Process.Capabilities.Bounding = append(s.Process.Capabilities.Bounding, "CAP_SYS_ADMIN")
		s.Linux.Resources = &specs.LinuxResources{Devices: []specs.LinuxDeviceCgroup{{Allow: true, Access: "rwm"}}}
		if ok, _, _ := Decide(s, Auto); ok {
			t.Fatal("privileged container got a user namespace")
		}
	})
	t.Run("NET_ADMIN in a pod network", func(t *testing.T) {
		ns := slices.Clone(ownNamespaces)
		ns[0].Path = "/var/run/netns/pod"
		s := podSpec(nil, ns...)
		s.Process.Capabilities.Bounding = append(s.Process.Capabilities.Bounding, "CAP_NET_ADMIN")
		if ok, _, _ := Decide(s, Auto); ok {
			t.Fatal("NET_ADMIN container in a pod network got a user namespace")
		}
	})
}

func TestDecideModes(t *testing.T) {
	pod := podSpec(map[string]string{"io.kubernetes.cri.container-type": "container", AnnotationHostUsers: "true"}, ownNamespaces...)
	if ok, _, err := Decide(pod, Always); !ok || err != nil {
		t.Fatalf("always: pod with opt-out annotation: %v, %v; want a user namespace", ok, err)
	}
	if ok, _, _ := Decide(podSpec(nil, ownNamespaces...), Never); ok {
		t.Fatal("never: got a user namespace")
	}
	priv := podSpec(nil, ownNamespaces...)
	priv.Process.Capabilities.Bounding = append(priv.Process.Capabilities.Bounding, "CAP_SYS_ADMIN")
	priv.Linux.Resources = &specs.LinuxResources{Devices: []specs.LinuxDeviceCgroup{{Allow: true, Access: "rwm"}}}
	if _, _, err := Decide(priv, Always); !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("always: privileged container: %v, want refused", err)
	}
	own := podSpec(nil, append(slices.Clone(ownNamespaces), specs.LinuxNamespace{Type: specs.UserNamespace})...)
	if ok, _, err := Decide(own, Always); ok || err != nil {
		t.Fatalf("always: spec with its own user namespace: %v, %v; want left alone", ok, err)
	}
	if err := Mode("sometimes").Validate(); err == nil {
		t.Fatal("bad mode accepted")
	}
}

func TestAllocate(t *testing.T) {
	stateDir = t.TempDir()
	pod := map[string]string{"io.kubernetes.cri.sandbox-id": "sb1"}

	a, err := Allocate(podSpec(pod), "k8s.io", "pause", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Allocate(podSpec(pod), "k8s.io", "app", 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Base != b.Base || a.Base != firstBase {
		t.Fatalf("pod containers got ranges %d and %d, want both %d", a.Base, b.Base, firstBase)
	}
	other, err := Allocate(podSpec(nil), "default", "solo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if other.Base != firstBase+Size {
		t.Fatalf("second pod got %d, want %d", other.Base, firstBase+Size)
	}

	// Asking for a range another pod holds fails; a free one is granted.
	if _, err := Allocate(podSpec(nil), "default", "x", a.Base); !errdefs.IsConflict(err) {
		t.Fatalf("claiming a used range: %v, want conflict", err)
	}
	want := uint32(firstBase + 5*Size)
	got, err := Allocate(podSpec(nil), "default", "y", want)
	if err != nil || got.Base != want {
		t.Fatalf("claiming a free range: %d, %v", got.Base, err)
	}

	// The range is freed only when the pod's last container is released.
	if err := Release(a, "pause"); err != nil {
		t.Fatal(err)
	}
	if _, err := Allocate(podSpec(nil), "default", "z", a.Base); !errdefs.IsConflict(err) {
		t.Fatalf("range freed while a pod container still uses it: %v", err)
	}
	if err := Release(b, "app"); err != nil {
		t.Fatal(err)
	}
	if got, err := Allocate(podSpec(nil), "default", "z", a.Base); err != nil || got.Base != a.Base {
		t.Fatalf("range not freed after the pod's last container: %d, %v", got.Base, err)
	}
}

func TestParseBase(t *testing.T) {
	for in, ok := range map[string]bool{
		"1073741824":           true,
		"1073807360\n":         true,
		"1073741825":           false, // not on a range boundary
		"0":                    false,
		"2147483648":           false, // past the last range
		"not a number":         false,
		"18446744073709551616": false,
	} {
		if _, err := ParseBase(in); (err == nil) != ok {
			t.Errorf("ParseBase(%q) error = %v, want ok=%v", in, err, ok)
		}
	}
}

func TestApply(t *testing.T) {
	tmpfs := t.TempDir() // on tmpfs or ext4 in CI; both are idmappable
	s := podSpec(nil, specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: "/var/run/netns/pod"})
	s.Mounts = []specs.Mount{
		{Destination: "/sys", Type: "sysfs", Source: "sysfs"},
		{Destination: "/data", Type: "bind", Source: tmpfs, Options: []string{"rbind"}},
		{Destination: "/proc", Type: "proc", Source: "proc"},
	}
	r := Range{Base: firstBase, Key: "k"}
	Apply(s, r)

	last := s.Linux.Namespaces[len(s.Linux.Namespaces)-1]
	if last.Type != specs.UserNamespace || last.Path != "" {
		t.Fatalf("user namespace not added: %+v", last)
	}
	if m := s.Linux.UIDMappings; len(m) != 1 || m[0].HostID != firstBase || m[0].Size != Size {
		t.Fatalf("uid mappings %+v", m)
	}
	if s.Mounts[0].Type != "bind" || s.Mounts[0].Source != "/sys" || !slices.Contains(s.Mounts[0].Options, "ro") {
		t.Fatalf("sysfs in a pod network not replaced by a read-only bind: %+v", s.Mounts[0])
	}
	if !idmappable(tmpfs) {
		t.Skip("temp dir filesystem does not support idmapped mounts")
	}
	if !slices.Contains(s.Mounts[1].Options, "idmap") {
		t.Fatalf("bind mount not idmapped: %+v", s.Mounts[1])
	}
	if slices.Contains(s.Mounts[2].Options, "idmap") {
		t.Fatalf("proc mount idmapped: %+v", s.Mounts[2])
	}
}

func TestDelegateCgroup(t *testing.T) {
	spec := func(caps ...string) *specs.Spec {
		return &specs.Spec{
			Process: &specs.Process{Capabilities: &specs.LinuxCapabilities{Bounding: caps}},
			Linux:   &specs.Linux{UIDMappings: []specs.LinuxIDMapping{{ContainerID: 0, HostID: 1 << 30, Size: 65536}}},
			Mounts:  []specs.Mount{{Destination: "/sys/fs/cgroup", Type: "cgroup", Options: []string{"nosuid", "ro"}}},
		}
	}
	s := spec("CAP_SYS_ADMIN")
	if got := DelegateCgroup(s); got != 1<<30 {
		t.Fatalf("owner %d, want %d", got, 1<<30)
	}
	if !slices.Equal(s.Mounts[0].Options, []string{"nosuid"}) {
		t.Fatalf("cgroup mount options %v", s.Mounts[0].Options)
	}
	if s := spec(); DelegateCgroup(s) != 0 || len(s.Mounts[0].Options) != 2 {
		t.Fatal("delegated without CAP_SYS_ADMIN")
	}
	if s := spec("CAP_SYS_ADMIN"); func() int { s.Linux.UIDMappings = nil; return DelegateCgroup(s) }() != 0 {
		t.Fatal("delegated without a user namespace")
	}
}

// A fork joins its snapshot's range: while the range is held, and again
// once its holders are gone, unless another container got it meanwhile.
func TestJoin(t *testing.T) {
	stateDir = t.TempDir()
	parent, err := Allocate(podSpec(nil), "default", "parent", 0)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := Join(parent.Key, "fork", parent.Base)
	if err != nil || fork != parent {
		t.Fatalf("join a held range: %+v, %v", fork, err)
	}
	for _, id := range []string{"parent", "fork"} {
		if err := Release(parent, id); err != nil {
			t.Fatal(err)
		}
	}
	again, err := Join(parent.Key, "fork2", parent.Base)
	if err != nil || again != parent {
		t.Fatalf("join a released range: %+v, %v", again, err)
	}
	if err := Release(again, "fork2"); err != nil {
		t.Fatal(err)
	}
	if _, err := Allocate(podSpec(nil), "default", "other", parent.Base); err != nil {
		t.Fatal(err)
	}
	if _, err := Join(parent.Key, "fork3", parent.Base); !errdefs.IsConflict(err) {
		t.Fatalf("join a range given to another container: %v, want conflict", err)
	}
	if _, err := Join("default_unknown", "x", 0); err == nil {
		t.Fatal("joined a key with no range and no base")
	}
}

// A snapshot holds its range until it is released, or until it is gone
// and a range is next allocated.
func TestHoldForSnapshot(t *testing.T) {
	stateDir = t.TempDir()
	parent, err := Allocate(podSpec(nil), "default", "parent", 0)
	if err != nil {
		t.Fatal(err)
	}
	snap := t.TempDir()
	if err := os.WriteFile(filepath.Join(snap, KeyFile), []byte(parent.Key), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := HoldForSnapshot(snap, parent); err != nil {
		t.Fatal(err)
	}
	if err := Release(parent, "parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := Allocate(podSpec(nil), "default", "x", parent.Base); !errdefs.IsConflict(err) {
		t.Fatalf("range freed while a snapshot holds it: %v", err)
	}
	if err := ReleaseForSnapshot(snap, parent); err != nil {
		t.Fatal(err)
	}
	got, err := Allocate(podSpec(nil), "default", "x", parent.Base)
	if err != nil || got.Base != parent.Base {
		t.Fatalf("range not freed with the snapshot's release: %v", err)
	}

	// Deleted by hand: the next allocation notices (once a pull could
	// no longer be writing it).
	pullGrace = 0
	t.Cleanup(func() { pullGrace = 10 * time.Minute })
	snap2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(snap2, KeyFile), []byte(got.Key), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := HoldForSnapshot(snap2, got); err != nil {
		t.Fatal(err)
	}
	if err := Release(got, "x"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(snap2); err != nil {
		t.Fatal(err)
	}
	if again, err := Allocate(podSpec(nil), "default", "y", got.Base); err != nil || again.Base != got.Base {
		t.Fatalf("range of a deleted snapshot not freed: %v", err)
	}
}

// A pulled snapshot's key gets the range asked for if it is free, any
// free one if asked for none, and keeps it for the next snapshot of the
// same lineage.
func TestClaimForSnapshot(t *testing.T) {
	stateDir = t.TempDir()
	other, err := Allocate(podSpec(nil), "default", "other", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimForSnapshot("/s/a", "pulled_x", other.Base); !errdefs.IsConflict(err) {
		t.Fatalf("claiming a range another container has: %v", err)
	}
	a, err := ClaimForSnapshot("/s/a", "pulled_x", 0)
	if err != nil || a.Base == other.Base || a.Key != "pulled_x" {
		t.Fatalf("claiming any free range: %+v, %v", a, err)
	}
	b, err := ClaimForSnapshot("/s/b", "pulled_x", other.Base+Size*7)
	if err != nil || b != a {
		t.Fatalf("a second snapshot of the lineage: %+v, %v; want %+v", b, err, a)
	}
	free := uint32(firstBase + 9*Size)
	c, err := ClaimForSnapshot("/s/c", "pulled_y", free)
	if err != nil || c.Base != free {
		t.Fatalf("claiming a free range: %+v, %v", c, err)
	}
}
