//go:build linux

// Package timetravel records a timeline of checkpoints taken just before a
// container makes a system call with lasting effects, so it can be rewound.
package timetravel

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	// AnnotationTrigger enables time travel; its value names the trigger set.
	AnnotationTrigger = "gcr.dev/time-travel"
	// AnnotationInterval is the minimum time between two points (Go duration).
	AnnotationInterval = "gcr.dev/time-travel-interval"
	// AnnotationKeep is how many points to keep; older ones are deleted.
	AnnotationKeep = "gcr.dev/time-travel-keep"
)

// triggerSets maps an AnnotationTrigger value to the system calls that take a point.
var triggerSets = map[string][]string{
	"execve": {"execve", "execveat"},
}

type Policy struct {
	Syscalls []string
	Interval time.Duration
	Keep     int
}

// ParsePolicy reads the time-travel annotations; nil means time travel is off.
func ParsePolicy(annotations map[string]string) (*Policy, error) {
	set, ok := annotations[AnnotationTrigger]
	if !ok {
		return nil, nil
	}
	syscalls, ok := triggerSets[set]
	if !ok {
		return nil, fmt.Errorf("%s=%q: unknown trigger set: %w", AnnotationTrigger, set, errdefs.ErrInvalidArgument)
	}
	p := &Policy{Syscalls: syscalls, Interval: time.Second, Keep: 20}
	if v, ok := annotations[AnnotationInterval]; ok {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("%s=%q: %w", AnnotationInterval, v, errdefs.ErrInvalidArgument)
		}
		p.Interval = d
	}
	if v, ok := annotations[AnnotationKeep]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s=%q: %w", AnnotationKeep, v, errdefs.ErrInvalidArgument)
		}
		p.Keep = n
	}
	return p, nil
}

// AddTrigger makes the spec's seccomp filter hand the trigger system calls to
// the supervisor listening at listenerPath. Existing rules for those calls are
// replaced: a notification is the only action left to take on them.
func (p *Policy) AddTrigger(spec *specs.Spec, listenerPath string) {
	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}
	sc := spec.Linux.Seccomp
	if sc == nil {
		sc = &specs.LinuxSeccomp{DefaultAction: specs.ActAllow}
		spec.Linux.Seccomp = sc
	}
	var rules []specs.LinuxSyscall
	for _, r := range sc.Syscalls {
		r.Names = slices.DeleteFunc(slices.Clone(r.Names), func(n string) bool {
			return slices.Contains(p.Syscalls, n)
		})
		if len(r.Names) > 0 {
			rules = append(rules, r)
		}
	}
	sc.Syscalls = append(rules, specs.LinuxSyscall{Names: p.Syscalls, Action: specs.ActNotify})
	sc.ListenerPath = listenerPath
}
