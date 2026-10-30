//go:build e2e && linux

// Package timetravel checks time travel end to end: a counter that execs
// /bin/true every 0.2s (each exec can take a point) and deletes /keep at
// count 25. Rewinding to the first point must bring back an early count
// and /keep, keep the container running at its pace, and keep recording
// points afterwards.
package timetravel

import (
	"os"
	"strconv"
	"strings"
	"syscall"
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

	// The container deletes /keep at count 25, after writing 25 and
	// before writing 26: past 25 it is gone. How long that takes is the
	// machine's business, not the test's.
	framework.Eventually(t, time.Minute, "the container to pass count 25", func() bool {
		s, err := c.ReadFile("/tmp/count")
		n, convErr := strconv.Atoi(s)
		return err == nil && convErr == nil && n > 25
	})
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

	// It keeps counting, so its execs are neither held nor failing; on a
	// loaded machine that takes longer than the 4s it needs when idle.
	var later int
	framework.Eventually(t, time.Minute, "the counter to advance by 10 after the rewind", func() bool {
		later = c.ReadInt("/tmp/count")
		return later-after >= 10
	})
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

// TestLimits: the handler bounds how often points are taken and how many
// are kept, each a dump of the container's memory on the node's disk.
func TestLimits(t *testing.T) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	for name, a := range map[string]map[string]string{
		"too often": {timetravel.AnnotationTrigger: "execve", timetravel.AnnotationInterval: "0s"},
		"too many":  {timetravel.AnnotationTrigger: "execve", timetravel.AnnotationKeep: "1000"},
	} {
		c := ns.Create(framework.UniqueID("tt-limit"), framework.Spec{Args: []string{"sleep", "60"}, Annotations: a})
		if err := c.Start(); err == nil || !strings.Contains(err.Error(), "TimeTravel") {
			t.Errorf("%s: %v, want refused by the handler's TimeTravel", name, err)
		}
	}
	// A handler that allows it.
	c := ns.Create(framework.UniqueID("tt-allowed"), framework.Spec{
		Args:          []string{"sleep", "60"},
		Annotations:   map[string]string{timetravel.AnnotationTrigger: "execve", timetravel.AnnotationKeep: "1000"},
		RuntimeConfig: "[TimeTravel]\nMaxKeep = 1000\n",
	})
	if err := c.Start(); err != nil {
		t.Fatalf("allowed by the handler: %v", err)
	}
}

// TestInitExec: the container's own process (pid 1) execs, looking the
// program up in PATH, as `sh -c "exec ..."` does. The lookup's failed
// execs are not points and are not held, the exec that succeeds is a
// point, and the container stops as promptly as any other, its exit
// reported.
func TestInitExec(t *testing.T) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	c := ns.Run(framework.UniqueID("tt-init"), framework.Spec{
		Annotations: map[string]string{
			timetravel.AnnotationTrigger:  "execve",
			timetravel.AnnotationInterval: "100ms",
		},
		// The sleep before the exec lets the recorder see the container
		// running.
		Args: []string{"sh", "-c", `sleep 0.5; exec sh -c "echo ready > /ready; exec sleep 3600"`},
	})
	start := time.Now()
	framework.Eventually(t, 30*time.Second, "the exec'd shell to run", func() bool {
		_, err := c.ReadFile("/ready")
		return err == nil
	})
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("pid 1's execs took %v", d.Round(time.Millisecond))
	}
	ctl := control.NewClient(ns.Name, c.ID)
	points, err := ctl.Timeline(ns.Ctx)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	initPoint := 0
	for _, p := range points {
		if !strings.HasPrefix(p.Path, "/bin/") && !strings.HasPrefix(p.Path, "/usr/bin/") {
			t.Errorf("point #%d of an exec that failed: %s", p.ID, p.Path)
		}
		if p.StoppedPid == 1 && len(p.Argv) > 0 && p.Argv[0] == "sh" {
			initPoint = p.ID
		}
	}
	if initPoint == 0 {
		t.Fatalf("no point at pid 1's exec: %+v", points)
	}

	// Rewound to it, the exec'd shell runs again from its first
	// instruction: /ready, written after the point, is written again.
	if err := ctl.Rewind(ns.Ctx, initPoint); err != nil {
		t.Fatalf("rewind to pid 1's exec: %v", err)
	}
	framework.Eventually(t, 10*time.Second, "the exec'd shell to run again", func() bool {
		_, err := c.ReadFile("/ready")
		return err == nil
	})
	if st := c.Status(); st != containerd.Running {
		t.Fatalf("task is %s after rewind", st)
	}

	exit, err := c.Task.Wait(ns.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Task.Kill(ns.Ctx, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case st := <-exit:
		if code, _, _ := st.Result(); code != 128+uint32(syscall.SIGKILL) {
			t.Errorf("exit %d after SIGKILL", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the container did not exit after SIGKILL")
	}
}
