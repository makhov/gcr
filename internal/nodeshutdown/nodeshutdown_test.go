package nodeshutdown

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPodUID(t *testing.T) {
	pod := func(optIn, kind, uid string) map[string]string {
		return map[string]string{AnnotationOptIn: optIn, "io.kubernetes.cri.container-type": kind, "io.kubernetes.cri.sandbox-uid": uid}
	}
	for name, tc := range map[string]struct {
		annotations map[string]string
		want        string
	}{
		"opted in":       {pod("true", "container", "u1"), "u1"},
		"not opted in":   {pod("", "container", "u1"), ""},
		"the sandbox":    {pod("true", "sandbox", "u1"), ""},
		"not a pod's":    {map[string]string{AnnotationOptIn: "true"}, ""},
		"a path for UID": {pod("true", "container", "../x"), ""},
		"a dot first":    {pod("true", "container", ".x"), ""},
	} {
		if got := PodUID(tc.annotations); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestStates(t *testing.T) {
	Dir = filepath.Join(t.TempDir(), "shutdown")
	if ShuttingDown() {
		t.Fatal("shutting down before Begin")
	}
	if states, err := States(); err != nil || len(states) != 0 {
		t.Fatalf("before Begin: %v, %v", states, err)
	}
	if err := Begin(); err != nil {
		t.Fatal(err)
	}
	if !ShuttingDown() {
		t.Fatal("not shutting down after Begin")
	}
	for _, step := range [][2]string{{"a", Pending}, {"b", Pending}, {"a", Ready}, {"b", Failed}, {"a", Pushed}} {
		if err := Set(step[0], step[1], "why"); err != nil {
			t.Fatal(err)
		}
	}
	states, err := States()
	if err != nil || len(states) != 2 || states["a"] != Pushed || states["b"] != Failed {
		t.Fatalf("states %v, %v; want a pushed, b failed", states, err)
	}
	if why, _ := os.ReadFile(filepath.Join(Dir, "b"+Failed)); string(why) != "why" {
		t.Errorf("failed says %q", why)
	}
	if err := Cancel(); err != nil || ShuttingDown() {
		t.Fatalf("after Cancel: %v, shutting down %v", err, ShuttingDown())
	}
}

// Await: gone once migrated; checkpointed on the operator's word, with no
// word within PlanWait, or with a migration planned but not ended within
// LiveWait; never while one is under way.
func TestAwait(t *testing.T) {
	Dir = filepath.Join(t.TempDir(), "shutdown")
	PlanWait, LiveWait, ReleaseWait = 300*time.Millisecond, 900*time.Millisecond, 600*time.Millisecond
	never := func() bool { return false }
	after := func(d time.Duration) func() bool {
		at := time.Now().Add(d)
		return func() bool { return time.Now().After(at) }
	}
	timed := func(f func() bool) (bool, time.Duration) {
		start := time.Now()
		ok := f()
		return ok, time.Since(start)
	}
	if ok, took := timed(func() bool { return Await("none", never, never) }); ok || took < PlanWait {
		t.Errorf("no plan: %v after %s, want a checkpoint after %s", ok, took, PlanWait)
	}
	_ = Plan("cp", Checkpoint)
	if ok, took := timed(func() bool { return Await("cp", never, never) }); ok || took > PlanWait/2 {
		t.Errorf("checkpoint planned: %v after %s, want one at once", ok, took)
	}
	_ = Plan("live", Live)
	// Migrated, not released: held until ReleaseWait.
	if ok, took := timed(func() bool { return Await("live", after(200*time.Millisecond), never) }); !ok || took < 200*time.Millisecond+ReleaseWait {
		t.Errorf("live, migrated, not released: %v after %s, want held %s more", ok, took, ReleaseWait)
	}
	go func() { time.Sleep(400 * time.Millisecond); _ = Plan("live", Released) }()
	if ok, took := timed(func() bool { return Await("live", after(200*time.Millisecond), never) }); !ok || took > 200*time.Millisecond+ReleaseWait {
		t.Errorf("live, migrated, released: %v after %s", ok, took)
	}
	_ = os.Remove(filepath.Join(Dir, "live"+Released))
	if ok, took := timed(func() bool { return Await("live", never, never) }); ok || took < LiveWait {
		t.Errorf("live, never migrated: %v after %s, want a checkpoint after %s", ok, took, LiveWait)
	}
	// Under way past LiveWait: it waits for the migration's end.
	start := time.Now()
	stillBusy := func() bool { return time.Since(start) < 1200*time.Millisecond }
	if ok, took := timed(func() bool { return Await("live", never, stillBusy) }); ok || took < 1200*time.Millisecond {
		t.Errorf("migrating past LiveWait: %v after %s, want to wait for it", ok, took)
	}
	// A plan changes: the other goes.
	_ = Plan("live", Checkpoint)
	if planned("live", Live) || !planned("live", Checkpoint) {
		t.Error("the plan did not change")
	}
	if err := Plan("../x", Live); err == nil {
		t.Error("a path as a pod UID")
	}
}
