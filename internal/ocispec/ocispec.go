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

// The annotations CRI implementations put on every pod container:
// containerd's, then CRI-O's.
var (
	containerTypeAnnotations = []string{"io.kubernetes.cri.container-type", "io.kubernetes.cri-o.ContainerType"}
	sandboxIDAnnotations     = []string{"io.kubernetes.cri.sandbox-id", "io.kubernetes.cri-o.SandboxID"}
)

// PodContainer says whether spec is a Kubernetes pod's container (its
// sandbox included), as containerd's or CRI-O's CRI marks them.
func PodContainer(spec *specs.Spec) bool {
	return firstAnnotation(spec, containerTypeAnnotations) != ""
}

// SandboxID is the pod a container belongs to, "" outside one.
func SandboxID(spec *specs.Spec) string {
	return firstAnnotation(spec, sandboxIDAnnotations)
}

func firstAnnotation(spec *specs.Spec, keys []string) string {
	for _, k := range keys {
		if v := spec.Annotations[k]; v != "" {
			return v
		}
	}
	return ""
}
