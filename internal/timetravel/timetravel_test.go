//go:build linux

package timetravel

import (
	"os"
	"slices"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestParsePolicy(t *testing.T) {
	if p, err := ParsePolicy(map[string]string{}); p != nil || err != nil {
		t.Fatalf("no annotation: %v, %v", p, err)
	}
	p, err := ParsePolicy(map[string]string{AnnotationTrigger: "execve", AnnotationInterval: "250ms", AnnotationKeep: "3"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Interval != 250*time.Millisecond || p.Keep != 3 || !slices.Equal(p.Syscalls, []string{"execve", "execveat"}) {
		t.Fatalf("policy %+v", p)
	}
	for _, bad := range []map[string]string{
		{AnnotationTrigger: "everything"},
		{AnnotationTrigger: "execve", AnnotationInterval: "soon"},
		{AnnotationTrigger: "execve", AnnotationKeep: "0"},
	} {
		if _, err := ParsePolicy(bad); err == nil {
			t.Errorf("ParsePolicy(%v) accepted", bad)
		}
	}
}

func TestAddTrigger(t *testing.T) {
	p := &Policy{Syscalls: []string{"execve", "execveat"}}

	// No profile: allow everything except the trigger calls.
	s := &specs.Spec{}
	p.AddTrigger(s)
	sc := s.Linux.Seccomp
	if sc.DefaultAction != specs.ActAllow || len(sc.Syscalls) != 1 || sc.Syscalls[0].Action != specs.ActNotify {
		t.Fatalf("seccomp %+v", sc)
	}

	// An existing profile keeps its rules, minus any for the trigger calls.
	s = &specs.Spec{Linux: &specs.Linux{Seccomp: &specs.LinuxSeccomp{
		DefaultAction: specs.ActErrno,
		Syscalls: []specs.LinuxSyscall{
			{Names: []string{"read", "execve", "write"}, Action: specs.ActAllow},
			{Names: []string{"execveat"}, Action: specs.ActAllow},
		},
	}}}
	p.AddTrigger(s)
	got := s.Linux.Seccomp.Syscalls
	if len(got) != 2 || !slices.Equal(got[0].Names, []string{"read", "write"}) || got[1].Action != specs.ActNotify {
		t.Fatalf("rules %+v", got)
	}
}

func TestTimeline(t *testing.T) {
	tl, err := NewTimeline(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for i := 0; i < 3; i++ {
		id, dir, err := tl.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		if err := tl.Commit(Point{ID: id, Syscall: "execve"}); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	pts := tl.Points()
	if len(pts) != 2 || pts[0].ID != 2 || pts[1].ID != 3 {
		t.Fatalf("points %+v, want #2 and #3", pts)
	}
	if _, err := os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Fatalf("pruned point's directory still exists: %v", err)
	}
	if _, err := tl.Get(1); err == nil {
		t.Fatal("pruned point still found")
	}

	id, dir, _ := tl.Reserve()
	tl.Discard(id)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("discarded point's directory still exists")
	}
}

// Incremental points need their parents: old ones go a chain at a time,
// never leaving fewer than Keep.
func TestTimelineChains(t *testing.T) {
	tl, err := NewTimeline(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	commit := func(parent int) int {
		t.Helper()
		id, _, err := tl.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		if err := tl.Commit(Point{ID: id, Parent: parent}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ids := func() []int {
		var out []int
		for _, p := range tl.Points() {
			out = append(out, p.ID)
		}
		return out
	}
	a := commit(0)
	b := commit(a)
	c := commit(b)
	d := commit(c) // one chain of 4, keep 3: nothing can go
	if got := ids(); !slices.Equal(got, []int{a, b, c, d}) {
		t.Fatalf("one chain: %v", got)
	}
	e := commit(0)
	f := commit(e)
	if got := ids(); !slices.Equal(got, []int{a, b, c, d, e, f}) {
		t.Fatalf("a second chain of 2: %v; the first must stay", got)
	}
	g := commit(f) // the second chain has 3: the first goes
	if got := ids(); !slices.Equal(got, []int{e, f, g}) {
		t.Fatalf("a second chain of 3: %v, want %v", got, []int{e, f, g})
	}
	for _, id := range []int{a, b, c, d} {
		if _, err := os.Stat(tl.Dir(id)); !os.IsNotExist(err) {
			t.Fatalf("point %d's directory kept: %v", id, err)
		}
	}
}
