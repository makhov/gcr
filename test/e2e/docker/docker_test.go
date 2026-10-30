//go:build e2e && linux

// Package docker checks gcr as a Docker runtime (docker run --runtime gcr):
// Docker's own operations, and gcr's snapshots, forks and time travel on
// containers Docker runs. It needs dockerd with the runtime registered
// (hack/docker-setup.sh) and skips without.
package docker

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/snapshot"
	"github.com/makhov/gcr/internal/timetravel"
	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

// namespace is the containerd namespace Docker's containers are in.
const namespace = "moby"

func docker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func must(t *testing.T, args ...string) string {
	t.Helper()
	out, err := docker(t, args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// skipWithoutDocker skips unless dockerd runs with the gcr runtime.
func skipWithoutDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	if out, err := docker(t, "info", "--format", "{{json .Runtimes}}"); err != nil || !strings.Contains(out, `"gcr"`) {
		t.Skip("no dockerd with the gcr runtime (hack/docker-setup.sh)")
	}
}

// run starts a container under gcr, removed when the test ends, and
// returns its full ID.
func run(t *testing.T, args ...string) string {
	t.Helper()
	name := framework.UniqueID("dk")
	id := must(t, append([]string{"run", "-d", "--name", name, "--runtime", "gcr"}, args...)...)
	t.Cleanup(func() { _, _ = docker(t, "rm", "-f", id) })
	return id
}

const counter = `trap "exit 0" TERM; echo keep > /keep; i=0; while :; do i=$((i+1)); echo $i > /count; /bin/true; sleep 0.1; done`

func count(t *testing.T, id string) int {
	t.Helper()
	out := must(t, "exec", id, "cat", "/count")
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("count: %q", out)
	}
	return n
}

func TestBasics(t *testing.T) {
	skipWithoutDocker(t)
	if out := must(t, "run", "--rm", "--runtime", "gcr", "busybox", "echo", "hi"); out != "hi" {
		t.Fatalf("run: %q", out)
	}
	out, err := docker(t, "run", "--rm", "--runtime", "gcr", "busybox", "sh", "-c", "exit 7")
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Fatalf("exit code: %v %q", err, out)
	}
	// Root in the container is not the host's: gcr's user namespace.
	if out := must(t, "run", "--rm", "--runtime", "gcr", "busybox", "cat", "/proc/self/uid_map"); strings.Fields(out)[1] == "0" {
		t.Fatalf("no user namespace: %q", out)
	}

	id := run(t, "-p", "127.0.0.1::80", "nginx:alpine")
	port := must(t, "port", id, "80")
	url := "http://" + port[strings.LastIndex(port, "\n")+1:] + "/"
	framework.Eventually(t, 30*time.Second, "nginx to answer on "+url, func() bool {
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode == http.StatusOK
	})
	if out := must(t, "exec", id, "nginx", "-v"); !strings.Contains(out, "nginx version") {
		t.Fatalf("exec: %q", out)
	}
	start := time.Now()
	must(t, "stop", id)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("stop took %v", d)
	}
	must(t, "start", id) // again, from the writable layer as left
}

// TestFork: a container on Docker's default bridge network is snapshotted
// and forked. The fork goes on from the snapshot's state, with an address
// of its own Docker gives it, and writes only its own layer.
func TestFork(t *testing.T) {
	skipWithoutDocker(t)
	parent := run(t, "busybox", "sh", "-c", counter)
	framework.Eventually(t, 10*time.Second, "the parent to count", func() bool { return count(t, parent) > 5 })

	dir := filepath.Join("/var/lib/gcr/snapshots", framework.UniqueID("docker"))
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := control.NewClient(namespace, parent).Snapshot(context.Background(), dir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Cleanup(func() { _ = snapshot.Remove(dir) })
	b, err := os.ReadFile(filepath.Join(dir, "upper", "count"))
	if err != nil {
		t.Fatal(err)
	}
	at, _ := strconv.Atoi(strings.TrimSpace(string(b)))

	fork := run(t, "--annotation", "gcr.dev/fork-from="+dir, "busybox", "true")
	if n := count(t, fork); n < at {
		t.Fatalf("the fork counts from %d, its snapshot was at %d", n, at)
	}
	addr := func(id string) string {
		return must(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", id)
	}
	if a, b := addr(parent), addr(fork); a == b || b == "" {
		t.Fatalf("parent and fork addresses: %q, %q", a, b)
	}
	if out := must(t, "exec", fork, "ip", "-o", "-4", "addr", "show", "eth0"); !strings.Contains(out, addr(fork)+"/") {
		t.Fatalf("the fork's eth0 is not Docker's address %s: %q", addr(fork), out)
	}
	must(t, "exec", fork, "sh", "-c", "echo mine > /mine")
	if _, err := docker(t, "exec", parent, "test", "-e", "/mine"); err == nil {
		t.Fatal("the fork's write reached the parent")
	}
}

// TestRewind: time travel on a container without a network of Docker's
// making; on a bridge network a rewind is refused before it stops
// anything.
func TestRewind(t *testing.T) {
	skipWithoutDocker(t)
	// Keeping more than the 20 by default: the first point, before /keep
	// is deleted, must still be there to go back to.
	tt := []string{"--annotation", timetravel.AnnotationTrigger + "=execve", "--annotation", timetravel.AnnotationInterval + "=200ms",
		"--annotation", timetravel.AnnotationKeep + "=50"}

	id := run(t, append(append([]string{"--network", "none"}, tt...), "busybox", "sh", "-c",
		`echo keep > /keep; i=0; while :; do i=$((i+1)); echo $i > /count; [ $i -eq 15 ] && rm /keep; /bin/true; sleep 0.2; done`)...)
	framework.Eventually(t, time.Minute, "the counter to pass 15", func() bool { return count(t, id) > 15 })
	ctl := control.NewClient(namespace, id)
	points, err := ctl.Timeline(context.Background())
	if err != nil || len(points) == 0 {
		t.Fatalf("timeline: %v, %d points", err, len(points))
	}
	before := count(t, id)
	if err := ctl.Rewind(context.Background(), points[0].ID); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if after := count(t, id); after >= before {
		t.Fatalf("count did not go back: %d -> %d", before, after)
	}
	must(t, "exec", id, "test", "-e", "/keep")
	if st := must(t, "inspect", "-f", "{{.State.Status}}", id); st != "running" {
		t.Fatalf("after the rewind: %s", st)
	}

	bridged := run(t, append(tt, "busybox", "sh", "-c", counter)...)
	framework.Eventually(t, 30*time.Second, "a point", func() bool {
		p, err := control.NewClient(namespace, bridged).Timeline(context.Background())
		return err == nil && len(p) > 0
	})
	p, _ := control.NewClient(namespace, bridged).Timeline(context.Background())
	err = control.NewClient(namespace, bridged).Rewind(context.Background(), p[0].ID)
	if err == nil || !strings.Contains(err.Error(), "--network none") {
		t.Fatalf("rewind on a bridge network: %v", err)
	}
	if st := must(t, "inspect", "-f", "{{.State.Status}}", bridged); st != "running" {
		t.Fatalf("after the refused rewind: %s", st)
	}
}
