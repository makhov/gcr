//go:build linux

package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"

	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/timetravel"
)

type fakeTimeline struct{ rewound int }

func (f *fakeTimeline) Points() []timetravel.Point {
	return []timetravel.Point{{ID: 7, Syscall: "execve", Path: "/bin/true"}}
}

func (f *fakeTimeline) Rewind(ctx context.Context, point int) error {
	if point != 7 {
		return errdefs.ErrNotFound
	}
	f.rewound = point
	return nil
}

func TestControlRoundTrip(t *testing.T) {
	socketDir = t.TempDir()
	ctx := context.Background()
	tl := &fakeTimeline{}
	srv, err := Serve(ctx, "ns", "c1", Handlers{
		Timeline: tl,
		Migrate: func(ctx context.Context, target, token string) (migrate.Result, error) {
			if token != "t" {
				return migrate.Result{}, errors.New("bad token")
			}
			return migrate.Result{Downtime: 42 * time.Millisecond}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c := NewClient("ns", "c1")
	pts, err := c.Timeline(ctx)
	if err != nil || len(pts) != 1 || pts[0].ID != 7 {
		t.Fatalf("timeline %+v, %v", pts, err)
	}
	if err := c.Rewind(ctx, 7); err != nil || tl.rewound != 7 {
		t.Fatalf("rewind: %v (rewound %d)", err, tl.rewound)
	}
	if err := c.Rewind(ctx, 8); err == nil {
		t.Fatal("rewind to a missing point succeeded")
	}
	res, err := c.Migrate(ctx, "http://target", "t")
	if err != nil || res.Downtime != 42*time.Millisecond {
		t.Fatalf("migrate %+v, %v", res, err)
	}
	if _, err := c.Migrate(ctx, "http://target", "x"); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("migrate error not passed through: %v", err)
	}
}

func TestControlWithoutTimeTravel(t *testing.T) {
	socketDir = t.TempDir()
	srv, err := Serve(context.Background(), "ns", "c2", Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	_, err = NewClient("ns", "c2").Timeline(context.Background())
	if err == nil || !strings.Contains(err.Error(), "time travel is off") {
		t.Fatalf("timeline without time travel: %v", err)
	}
}

func TestSocketPathFitsSunPath(t *testing.T) {
	socketDir = "/run/gcr-ctl"
	p := SocketPath("k8s.io", strings.Repeat("a", 64))
	if len(p) >= 108 {
		t.Fatalf("socket path %d bytes, sun_path holds 107", len(p))
	}
}
