//go:build e2e && linux

// Package timetravel checks time travel end to end: a counter that execs
// /bin/true every 0.2s (each exec can take a point) and deletes /keep at
// count 25. Rewinding to the first point must bring back an early count
// and /keep, keep the container running at its pace, and keep recording
// points afterwards.
package timetravel

import (
	"os"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"

	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/timetravel"
	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

func TestRewind(t *testing.T) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	c := ns.Run(framework.UniqueID("tt"), framework.Spec{
		Annotations: map[string]string{
			timetravel.AnnotationTrigger:  "execve",
			timetravel.AnnotationInterval: "1s",
		},
		Args: []string{"sh", "-c", `
			echo keep > /keep
			i=0
			while true; do
				i=$((i+1)); echo $i > /tmp/count.new && mv /tmp/count.new /tmp/count
				[ $i -eq 25 ] && rm /keep
				/bin/true
				sleep 0.2
			done`},
	})
	ctl := control.NewClient(ns.Name, c.ID)
	timeline := func() []timetravel.Point {
		t.Helper()
		points, err := ctl.Timeline(ns.Ctx)
		if err != nil {
			t.Fatalf("timeline: %v", err)
		}
		return points
	}

	time.Sleep(8 * time.Second)
	before := c.ReadInt("/tmp/count")
	if _, err := os.Stat(c.Rootfs("/keep")); !os.IsNotExist(err) {
		t.Fatalf("/keep should be gone at count %d", before)
	}
	points := timeline()
	if len(points) == 0 {
		t.Fatal("no points recorded")
	}
	first := points[0].ID
	for _, p := range points {
		first = min(first, p.ID)
	}
	t.Logf("count %d, /keep deleted, %d points; rewinding to #%d", before, len(points), first)

	if err := ctl.Rewind(ns.Ctx, first); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	rewound := time.Now()
	time.Sleep(500 * time.Millisecond)
	after := c.ReadInt("/tmp/count")
	if after >= before {
		t.Fatalf("count did not go back (%d -> %d)", before, after)
	}
	if _, err := os.Stat(c.Rootfs("/keep")); err != nil {
		t.Fatalf("/keep was not restored: %v", err)
	}
	if st := c.Status(); st != containerd.Running {
		t.Fatalf("task is %s after rewind", st)
	}

	time.Sleep(4 * time.Second)
	later := c.ReadInt("/tmp/count")
	// Same pace as before the rewind (~4/s): execs are not held or failing.
	if later-after < 10 {
		t.Fatalf("counter too slow after rewind (%d -> %d in 4s)", after, later)
	}
	// Points taken after the rewind prove the supervisor got the restored
	// listener.
	var fresh int
	for _, p := range timeline() {
		if p.Time.After(rewound) {
			fresh++
		}
	}
	if fresh < 2 {
		t.Fatalf("only %d points recorded after the rewind", fresh)
	}
	t.Logf("count %d right after the rewind, %d 4s later; %d new points", after, later, fresh)
}
