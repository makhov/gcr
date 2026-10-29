//go:build e2e && linux

package timetravel

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/timetravel"
	"github.com/makhov/gcr/test/e2e/framework"
)

// TestCost measures what time travel costs, for the tradeoffs doc. It is
// a benchmark, run with GCR_E2E_BENCH=1:
//
//   - every exec is handed to gcr's supervisor while time travel is on,
//     whether or not it takes a point;
//   - a point freezes the container while its writable layer is copied
//     and its memory dumped (Snapshot, which a point is), so it grows
//     with both, and takes as much disk.
func TestCost(t *testing.T) {
	if os.Getenv("GCR_E2E_BENCH") == "" {
		t.Skip("a benchmark: GCR_E2E_BENCH=1")
	}
	ns := framework.NewNamespace(t, "gcr-e2e")

	t.Run("exec", func(t *testing.T) {
		const n = 8000
		loop := fmt.Sprintf(`a=$(cut -d' ' -f1 /proc/uptime)
i=0; while [ $i -lt %d ]; do /bin/true; i=$((i+1)); done
b=$(cut -d' ' -f1 /proc/uptime); echo $a $b`, n)
		run := func(name string, annotations map[string]string) time.Duration {
			out, code := ns.Output(framework.UniqueID("exec-"+name), framework.Spec{
				Args: []string{"sh", "-c", loop}, Annotations: annotations,
			})
			f := strings.Fields(out)
			if code != 0 || len(f) != 2 {
				t.Fatalf("%s: exit %d: %s", name, code, out)
			}
			a, _ := strconv.ParseFloat(f[0], 64)
			b, _ := strconv.ParseFloat(f[1], 64)
			return time.Duration((b - a) * float64(time.Second))
		}
		tt := func(interval string) map[string]string {
			return map[string]string{timetravel.AnnotationTrigger: "execve", timetravel.AnnotationInterval: interval}
		}
		off := run("off", nil)
		once := run("once", tt("1h"))
		perSecond := run("1s", tt("1s"))
		per := func(d time.Duration) string { return (d / n).Round(time.Microsecond).String() }
		t.Logf("%d execs: off %s (%s each); on, one point %s (%s each); on, a point a second %s (%s each)",
			n, off.Round(time.Millisecond), per(off), once.Round(time.Millisecond), per(once),
			perSecond.Round(time.Millisecond), per(perSecond))
	})

	hog := framework.BuildStatic(t, "../fork/hog")
	point := func(t *testing.T, c *framework.Container, label string) {
		t.Helper()
		var took []time.Duration
		var size, images int64
		for i := range 3 {
			// Where time travel keeps them: disk, not the tmpfs /tmp may be.
			dir := filepath.Join(pointsDir(t), "point")
			begin := time.Now()
			if err := control.NewClient(ns.Name, c.ID).Snapshot(t.Context(), dir); err != nil {
				t.Fatalf("%s: snapshot: %v", label, err)
			}
			took = append(took, time.Since(begin))
			if i == 0 {
				size, images = du(t, dir), du(t, filepath.Join(dir, "images"))
			}
			_ = os.RemoveAll(dir)
		}
		slices.Sort(took)
		t.Logf("%s: a point freezes it for %s (of 3: %s to %s); %d MiB on disk, %d MiB of it memory",
			label, took[1].Round(time.Millisecond), took[0].Round(time.Millisecond), took[2].Round(time.Millisecond),
			size>>20, images>>20)
	}

	for _, mib := range []string{"64", "256", "1024"} {
		t.Run("memory-"+mib, func(t *testing.T) {
			c := ns.Run(framework.UniqueID("hog"), framework.Spec{
				Args:   []string{"/hog", mib},
				Mounts: []specs.Mount{framework.BindRO(hog, "/hog")},
			})
			framework.Eventually(t, 3*time.Minute, "the hog to fill its memory", func() bool {
				s, _ := c.ReadFile("/tmp/ready")
				return s == mib
			})
			point(t, c, mib+" MiB of memory")
			c.Remove()
		})
	}

	t.Run("writable-layer", func(t *testing.T) {
		c := ns.Run(framework.UniqueID("files"), framework.Spec{
			Args:   []string{"/hog", "64"},
			Mounts: []specs.Mount{framework.BindRO(hog, "/hog")},
		})
		framework.Eventually(t, time.Minute, "the hog to fill its memory", func() bool {
			s, _ := c.ReadFile("/tmp/ready")
			return s == "64"
		})
		if out, code := c.Exec("dd", "if=/dev/urandom", "of=/data", "bs=1M", "count=200"); code != 0 {
			t.Fatalf("write: %s", out)
		}
		point(t, c, "64 MiB of memory and 200 MiB written")
	})
}

func pointsDir(t *testing.T) string {
	t.Helper()
	if err := os.MkdirAll("/var/lib/gcr", 0o711); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/var/lib/gcr", "bench-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func du(t *testing.T, dir string) int64 {
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
