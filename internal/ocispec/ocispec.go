//go:build linux

// Package ocispec has small predicates over OCI runtime specs.
package ocispec

import (
	"slices"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// HasCapability reports whether cap is in the process's bounding set.
func HasCapability(spec *specs.Spec, cap string) bool {
	p := spec.Process
	return p != nil && p.Capabilities != nil && slices.Contains(p.Capabilities.Bounding, cap)
}

// Privileged reports a privileged container (docker or ctr --privileged,
// Kubernetes privileged: true): CAP_SYS_ADMIN and access to all devices,
// which containerd grants only to privileged containers. A container that
// only adds CAP_SYS_ADMIN is not privileged; in a user namespace that
// capability only reaches the namespace.
func Privileged(spec *specs.Spec) bool {
	return HasCapability(spec, "CAP_SYS_ADMIN") && allDevicesAllowed(spec)
}

// allDevicesAllowed reports a device cgroup rule allowing every device: no
// type (or "a") and no major or minor number.
func allDevicesAllowed(spec *specs.Spec) bool {
	if spec.Linux == nil || spec.Linux.Resources == nil {
		return false
	}
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && (d.Type == "" || d.Type == "a") && d.Major == nil && d.Minor == nil {
			return true
		}
	}
	return false
}
