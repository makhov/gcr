//go:build linux

package userns

import (
	"slices"
	"testing"

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
		{"kubernetes pod follows hostUsers", podSpec(map[string]string{containerTypeAnnotation: "container"}, ownNamespaces...), false},
		{"kubernetes pod opted in", podSpec(map[string]string{containerTypeAnnotation: "container", AnnotationHostUsers: "false"}, ownNamespaces...), true},
		{"own user namespace", podSpec(nil, append(slices.Clone(ownNamespaces), specs.LinuxNamespace{Type: specs.UserNamespace})...), false},
		{"host network (no entry)", podSpec(nil, specs.LinuxNamespace{Type: specs.PIDNamespace}, specs.LinuxNamespace{Type: specs.IPCNamespace}), false},
		{"host network (path)", podSpec(nil, specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: "/proc/1/ns/net"},
			specs.LinuxNamespace{Type: specs.PIDNamespace}, specs.LinuxNamespace{Type: specs.IPCNamespace}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := Wanted(tc.spec)
			if got != tc.want {
				t.Fatalf("Wanted = %v (%s), want %v", got, why, tc.want)
			}
		})
	}

	t.Run("privileged", func(t *testing.T) {
		s := podSpec(nil, ownNamespaces...)
		s.Process.Capabilities.Bounding = append(s.Process.Capabilities.Bounding, "CAP_SYS_ADMIN")
		if ok, _ := Wanted(s); ok {
			t.Fatal("privileged container got a user namespace")
		}
	})
	t.Run("NET_ADMIN in a pod network", func(t *testing.T) {
		ns := slices.Clone(ownNamespaces)
		ns[0].Path = "/var/run/netns/pod"
		s := podSpec(nil, ns...)
		s.Process.Capabilities.Bounding = append(s.Process.Capabilities.Bounding, "CAP_NET_ADMIN")
		if ok, _ := Wanted(s); ok {
			t.Fatal("NET_ADMIN container in a pod network got a user namespace")
		}
	})
}

func TestAllocate(t *testing.T) {
	stateDir = t.TempDir()
	pod := map[string]string{sandboxAnnotation: "sb1"}

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
