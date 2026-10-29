//go:build e2e && linux

package timetravel

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/timetravel"
	"github.com/makhov/gcr/test/e2e/framework"
)

// TestIncremental: where the kernel tracks written pages (not arm64),
// points after the first of a chain dump only the memory written since
// the last, and a rewind to one of them brings the whole memory back.
func TestIncremental(t *testing.T) {
	const mib = "256"
	ns := framework.NewNamespace(t, "gcr-e2e")
	hog := framework.BuildStatic(t, "../fork/hog")
	c := ns.Run(framework.UniqueID("tt-hog"), framework.Spec{
		Annotations: map[string]string{
			timetravel.AnnotationTrigger:  "execve",
			timetravel.AnnotationInterval: "1s",
		},
		// The hog holds the memory and checks it as it counts; the
		// loop's execs take the points.
		Args:   []string{"sh", "-c", "/hog " + mib + " & while true; do /bin/true; sleep 0.3; done"},
		Mounts: []specs.Mount{framework.BindRO(hog, "/hog")},
	})
	framework.Eventually(t, 2*time.Minute, "the hog to fill its memory", func() bool {
		s, _ := c.ReadFile("/tmp/ready")
		return s == mib
	})
	ready := time.Now()
	ctl := control.NewClient(ns.Name, c.ID)
	var points []timetravel.Point
	framework.Eventually(t, time.Minute, "three points after the memory is filled", func() bool {
		all, err := ctl.Timeline(ns.Ctx)
		if err != nil {
			t.Fatalf("timeline: %v", err)
		}
		points = points[:0]
		for _, p := range all {
			if p.Time.After(ready) {
				points = append(points, p)
			}
		}
		return len(points) >= 3
	})

	dir := func(id int) string {
		return filepath.Join("/var/lib/gcr", ns.Name, c.ID, "time-travel", "points", strconv.Itoa(id), "images")
	}
	last := points[len(points)-1]
	size := treeSize(t, dir(last.ID))
	t.Logf("point #%d (parent #%d): %d MiB of images, with %s MiB in memory", last.ID, last.Parent, size>>20, mib)
	if !container.MemoryTracking() {
		for _, p := range points {
			if p.Parent != 0 {
				t.Fatalf("point #%d is incremental without memory tracking", p.ID)
			}
		}
		t.Log("no memory tracking here: points are full")
	} else {
		if last.Parent == 0 {
			t.Fatalf("point #%d is full, mid-chain", last.ID)
		}
		if size > 32<<20 {
			t.Fatalf("incremental point #%d has %d MiB of images: more than what changed", last.ID, size>>20)
		}
		if _, err := os.Lstat(filepath.Join(dir(last.ID), "parent")); err != nil {
			t.Fatalf("point #%d has no parent link: %v", last.ID, err)
		}
	}

	// Back to that point: the memory comes from its whole chain.
	if err := ctl.Rewind(ns.Ctx, last.ID); err != nil {
		t.Fatalf("rewind to #%d: %v", last.ID, err)
	}
	at := 0
	framework.Eventually(t, time.Minute, "the hog to count on after the rewind", func() bool {
		s, _ := c.ReadFile("/tmp/count")
		if strings.HasPrefix(s, "corrupt") {
			t.Fatalf("memory came back wrong after the rewind: %s", s)
		}
		n, err := strconv.Atoi(s)
		if at == 0 && err == nil {
			at = n
		}
		return err == nil && n > at+5
	})
}

// treeSize is the size of the regular files under dir.
func treeSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
