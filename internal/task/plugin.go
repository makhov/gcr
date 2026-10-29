//go:build linux

package task

import (
	"context"
	"os"

	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.EventPlugin,
			plugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			pp, err := ic.GetByID(plugins.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}
			ss, err := ic.GetByID(plugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			publisher := pp.(shim.Publisher)
			if os.Getenv("TTRPC_ADDRESS") == "" {
				// CRI-O gives no address for events: it waits on tasks.
				publisher = discard{}
			}
			return NewTaskService(ic.Context, publisher, ss.(shutdown.Service))
		},
	})
}

// discard drops events, for callers that take none.
type discard struct{}

func (discard) Publish(context.Context, string, events.Event) error { return nil }
func (discard) Close() error                                        { return nil }
