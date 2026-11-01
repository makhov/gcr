package nodeshutdown

import (
	"os"
	"path/filepath"
	"testing"
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
