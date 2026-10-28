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
	masked := &specs.Linux{MaskedPaths: []string{"/proc/kcore"}}
	for _, tc := range []struct {
		name string
		spec *specs.Spec
		want bool
	}{
		{"privileged", &specs.Spec{Process: caps("CAP_SYS_ADMIN"), Linux: &specs.Linux{}}, true},
		{"cap added", &specs.Spec{Process: caps("CAP_SYS_ADMIN"), Linux: masked}, false},
		{"default caps", &specs.Spec{Process: caps("CAP_CHOWN"), Linux: &specs.Linux{}}, false},
	} {
		if got := Privileged(tc.spec); got != tc.want {
			t.Errorf("%s: Privileged = %v, want %v", tc.name, got, tc.want)
		}
	}
}
