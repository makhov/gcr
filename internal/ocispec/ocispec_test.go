//go:build linux

package ocispec

import (
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestPrivileged(t *testing.T) {
	caps := func(c ...string) *specs.Process {
		return &specs.Process{Capabilities: &specs.LinuxCapabilities{Bounding: c}}
	}
	devices := func(d ...specs.LinuxDeviceCgroup) *specs.Linux {
		return &specs.Linux{Resources: &specs.LinuxResources{Devices: d}}
	}
	all := specs.LinuxDeviceCgroup{Allow: true, Access: "rwm"}
	denyAll := specs.LinuxDeviceCgroup{Allow: false, Access: "rwm"}
	major := int64(1)
	one := specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: &major, Access: "rwm"}
	for _, tc := range []struct {
		name string
		spec *specs.Spec
		want bool
	}{
		{"privileged", &specs.Spec{Process: caps("CAP_SYS_ADMIN"), Linux: devices(denyAll, all)}, true},
		{"cap added", &specs.Spec{Process: caps("CAP_SYS_ADMIN"), Linux: devices(denyAll, one)}, false},
		{"cap added, no masked paths (crictl)", &specs.Spec{Process: caps("CAP_SYS_ADMIN"), Linux: &specs.Linux{}}, false},
		{"all devices, no CAP_SYS_ADMIN", &specs.Spec{Process: caps("CAP_CHOWN"), Linux: devices(all)}, false},
	} {
		if got := Privileged(tc.spec); got != tc.want {
			t.Errorf("%s: Privileged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPodContainer(t *testing.T) {
	for _, tc := range []struct {
		annotations map[string]string
		pod         bool
		sandbox     string
	}{
		{nil, false, ""},
		{map[string]string{"io.kubernetes.cri.container-type": "container", "io.kubernetes.cri.sandbox-id": "a"}, true, "a"},
		{map[string]string{"io.kubernetes.cri-o.ContainerType": "sandbox", "io.kubernetes.cri-o.SandboxID": "b"}, true, "b"},
	} {
		spec := &specs.Spec{Annotations: tc.annotations}
		if PodContainer(spec) != tc.pod || SandboxID(spec) != tc.sandbox {
			t.Errorf("%v: pod %v, sandbox %q", tc.annotations, PodContainer(spec), SandboxID(spec))
		}
	}
}
