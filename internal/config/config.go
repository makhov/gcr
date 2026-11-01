//go:build linux

// Package config reads gcr's per-runtime-handler configuration: the
// `options` table of the runtime in containerd's config (or the file its
// ConfigPath names), which containerd passes to the shim on create.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/api/types/runtimeoptions/v1"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/pelletier/go-toml/v2"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/hardening"
	"github.com/makhov/gcr/internal/landlock"
	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/netpolicy"
	"github.com/makhov/gcr/internal/userns"
)

// RestrictedAnnotations are the gcr.dev annotations a Kubernetes pod may
// use only where its handler lists them in PodAnnotations: with them a
// pod author picks a host directory to restore from (fork-from), and
// holds up the node's container creation waiting for it (fork-wait),
// makes the shim listen on the host's network for state to restore
// (migrate-in, migrate-token), or picks its host IDs (userns-base).
var RestrictedAnnotations = []string{
	container.AnnotationForkFrom,
	container.AnnotationForkWait,
	migrate.AnnotationIn,
	migrate.AnnotationToken,
	userns.AnnotationBase,
}

type Config struct {
	// UserNamespaces is "auto" (default), "always" or "never"; see
	// userns.Mode.
	UserNamespaces userns.Mode `toml:"UserNamespaces"`
	// Hardening is "standard" (default) or "off"; see hardening.Level.
	Hardening hardening.Level `toml:"Hardening"`
	// NestedUserNamespaces is "guarded" (default) or "deny"; see
	// hardening.NestedUserNamespaces.
	NestedUserNamespaces hardening.NestedUserNamespaces `toml:"NestedUserNamespaces"`
	// Landlock is "standard" (default) or "off"; see landlock.Level.
	Landlock landlock.Level `toml:"Landlock"`
	// NetlinkPolicy is "auto" (default) or "off"; see netpolicy.Mode.
	NetlinkPolicy netpolicy.Mode `toml:"NetlinkPolicy"`
	// MigrationTLS secures live migration to and from the handler's
	// containers; see migrate.TLS. Unset, migrations are plain HTTP.
	MigrationTLS migrate.TLS `toml:"MigrationTLS"`
	// PodAnnotations are the restricted gcr.dev annotations (see
	// RestrictedAnnotations) Kubernetes pods may use on this handler:
	// none by default, as a pod author could reach the node with them.
	PodAnnotations []string `toml:"PodAnnotations"`
	// SnapshotRoots are the directories snapshots may be forked from
	// (gcr.dev/fork-from), with every layer beneath them: required for
	// pods to fork at all, and checked for any container if set.
	SnapshotRoots []string `toml:"SnapshotRoots"`
	// TimeTravel limits what a container's annotations may ask of time
	// travel: every point is a dump of its memory, kept on the node.
	TimeTravel TimeTravelLimits `toml:"TimeTravel"`

	// IOUID and IOGID own the container's stdio pipes. containerd sets
	// them (in runc's options) for Kubernetes user namespace pods, so the
	// container's root can reopen /dev/stdout.
	IOUID, IOGID int `toml:"-"`
}

// TimeTravelLimits bound gcr.dev/time-travel-interval and -keep.
type TimeTravelLimits struct {
	// MinInterval is the shortest interval between points a container
	// may ask for (default 100ms).
	MinInterval Duration `toml:"MinInterval"`
	// MaxKeep is the most points a container may keep (default 50).
	MaxKeep int `toml:"MaxKeep"`
}

// Duration is a time.Duration written as Go writes them ("1s", "250ms").
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("%q: %w", b, errdefs.ErrInvalidArgument)
	}
	*d = Duration(v)
	return nil
}

// FromOptions decodes the runtime options of a create request. No options
// means the defaults.
func FromOptions(a *anypb.Any) (Config, error) {
	c := Config{UserNamespaces: userns.Auto, Hardening: hardening.Standard,
		NestedUserNamespaces: hardening.Guarded, Landlock: landlock.Standard,
		NetlinkPolicy: netpolicy.Auto,
		TimeTravel:    TimeTravelLimits{MinInterval: Duration(100 * time.Millisecond), MaxKeep: 50}}
	if a == nil || len(a.GetValue()) == 0 {
		return c, nil
	}
	v, err := typeurl.UnmarshalAny(a)
	if err != nil {
		return c, fmt.Errorf("runtime options: %w", err)
	}
	if ro, ok := v.(*runcoptions.Options); ok {
		// containerd's CRI sends runc's options for user namespace pods;
		// only the stdio owner applies to gcr.
		c.IOUID, c.IOGID = int(ro.IoUid), int(ro.IoGid)
		return c, nil
	}
	o, ok := v.(*runtimeoptions.Options)
	if !ok {
		return c, fmt.Errorf("runtime options are %T, want runtimeoptions: %w", v, errdefs.ErrInvalidArgument)
	}
	body := o.ConfigBody
	if o.ConfigPath != "" {
		if body, err = os.ReadFile(o.ConfigPath); err != nil {
			return c, fmt.Errorf("runtime config: %w", err)
		}
	}
	if err := toml.Unmarshal(body, &c); err != nil {
		return c, fmt.Errorf("runtime config: %w", err)
	}
	if err := c.UserNamespaces.Validate(); err != nil {
		return c, err
	}
	if err := c.Hardening.Validate(); err != nil {
		return c, err
	}
	if err := c.NestedUserNamespaces.Validate(); err != nil {
		return c, err
	}
	if err := c.Landlock.Validate(); err != nil {
		return c, err
	}
	if err := c.NetlinkPolicy.Validate(); err != nil {
		return c, err
	}
	if err := c.MigrationTLS.Validate(); err != nil {
		return c, err
	}
	for _, a := range c.PodAnnotations {
		if !slices.Contains(RestrictedAnnotations, a) {
			return c, fmt.Errorf("PodAnnotations: %q is not one of %v (the others need no listing): %w",
				a, RestrictedAnnotations, errdefs.ErrInvalidArgument)
		}
	}
	if c.TimeTravel.MinInterval < 0 || c.TimeTravel.MaxKeep < 1 {
		return c, fmt.Errorf("TimeTravel: MinInterval must be at least 0 and MaxKeep at least 1: %w", errdefs.ErrInvalidArgument)
	}
	for _, r := range c.SnapshotRoots {
		if !filepath.IsAbs(r) || filepath.Clean(r) == "/" {
			return c, fmt.Errorf("SnapshotRoots: %q must be an absolute path below /: %w", r, errdefs.ErrInvalidArgument)
		}
	}
	return c, nil
}
