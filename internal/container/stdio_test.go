//go:build linux

package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// emit writes to an incarnation's output ends, as its process would, and
// lets the copies finish.
func emit(t *testing.T, s *stdio, out, errw string) {
	t.Helper()
	_, o, e, err := s.attach(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Write([]byte(out)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]byte(errw)); err != nil {
		t.Fatal(err)
	}
	s.detach()
	s.wait()
}

func TestStdioFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "c.log")
	uri := "file://" + path
	s, err := openStdio(t.Context(), "c", "", uri, uri)
	if err != nil {
		t.Fatal(err)
	}
	emit(t, s, "out\n", "err\n")
	s.Close()
	b, _ := os.ReadFile(path)
	// One file, in either order: two copies write it.
	if got := string(b); got != "out\nerr\n" && got != "err\nout\n" {
		t.Errorf("file holds %q", got)
	}
}

func TestStdioBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "logger")
	// A logger as nerdctl's: stdout on fd 3, stderr on fd 4, ready once fd
	// 5 is closed; this one records its arguments and environment too. On
	// SIGTERM it flushes, as a logger should: its readers end at EOF.
	script := `#!/bin/sh
trap "" TERM
d=$(dirname "$0")
echo "$* $CONTAINER_ID $CONTAINER_NAMESPACE" >"$d/args"
cat <&3 >"$d/out" 4<&- 5>&- & cat <&4 >"$d/err" 3<&- 5>&- &
exec 3<&- 4<&- 5>&-
wait
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := namespaces.WithNamespace(context.Background(), "ns1")
	uri := "binary://" + bin + "?level=debug"
	s, err := openStdio(ctx, "c1", "", uri, uri)
	if err != nil {
		t.Fatal(err)
	}
	emit(t, s, "to stdout\n", "to stderr\n")
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(loggerStopTimeout + 5*time.Second):
		t.Fatal("closing did not end the logger")
	}
	for f, want := range map[string]string{"args": "level debug c1 ns1\n", "out": "to stdout\n", "err": "to stderr\n"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if string(b) != want {
			t.Errorf("logger's %s: %q, want %q", f, b, want)
		}
	}
}

func TestStdioBinaryV2NotReady(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "logger")
	// Exits without writing the readiness byte binary-v2 asks for.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := openStdio(t.Context(), "c", "", "binary-v2://"+bin, "binary-v2://"+bin)
	if err == nil || !strings.Contains(err.Error(), "before it was ready") {
		t.Fatalf("got %v, want a logger that was not ready", err)
	}
}

func TestStdioUnknownScheme(t *testing.T) {
	if _, err := openStdio(t.Context(), "c", "", "tcp://x", "tcp://x"); err == nil {
		t.Fatal("tcp:// accepted")
	}
}
