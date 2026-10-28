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

// Privileged reports a privileged container (docker --privileged,
// Kubernetes privileged: true): CAP_SYS_ADMIN and no masked /proc paths.
// A container that only adds CAP_SYS_ADMIN keeps its masked paths; in a
// user namespace that capability is scoped to the namespace.
func Privileged(spec *specs.Spec) bool {
	return HasCapability(spec, "CAP_SYS_ADMIN") && (spec.Linux == nil || len(spec.Linux.MaskedPaths) == 0)
}
