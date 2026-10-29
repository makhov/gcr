//go:build linux

// Package config reads gcr's per-runtime-handler configuration: the
// `options` table of the runtime in containerd's config (or the file its
// ConfigPath names), which containerd passes to the shim on create.
package config

import (
	"fmt"
	"os"

	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/api/types/runtimeoptions/v1"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/pelletier/go-toml/v2"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/makhov/gcr/internal/hardening"
	"github.com/makhov/gcr/internal/landlock"
	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/netpolicy"
	"github.com/makhov/gcr/internal/userns"
)

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

	// IOUID and IOGID own the container's stdio pipes. containerd sets
	// them (in runc's options) for Kubernetes user namespace pods, so the
	// container's root can reopen /dev/stdout.
	IOUID, IOGID int `toml:"-"`
}

// FromOptions decodes the runtime options of a create request. No options
// means the defaults.
func FromOptions(a *anypb.Any) (Config, error) {
	c := Config{UserNamespaces: userns.Auto, Hardening: hardening.Standard,
		NestedUserNamespaces: hardening.Guarded, Landlock: landlock.Standard,
		NetlinkPolicy: netpolicy.Auto}
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
	return c, nil
}
