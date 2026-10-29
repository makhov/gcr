//go:build e2e && linux

package fork

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/userns"
	"github.com/makhov/gcr/test/e2e/framework"
)

// pod marks a container as a Kubernetes pod's, as CRI does.
var pod = map[string]string{"io.kubernetes.cri.container-type": "container"}

func podWith(extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range pod {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// TestForkPolicy: a pod author chooses gcr.dev annotations, so the
// restricted ones are refused unless the handler lists them, and a pod
// forks only from gcr's own snapshots under SnapshotRoots.
func TestForkPolicy(t *testing.T) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	// Host users: a pod (without hostUsers: false) gets no user
	// namespace, so neither does the snapshot it forks from.
	parent := ns.Run(framework.UniqueID("parent"), framework.Spec{
		Args:        []string{"sh", "-c", counter},
		Annotations: map[string]string{userns.AnnotationHostUsers: "true"},
	})
	framework.Eventually(t, time.Minute, "the parent to count", func() bool {
		s, _ := parent.ReadFile("/tmp/count")
		n, err := strconv.Atoi(s)
		return err == nil && n > 0
	})
	root := framework.TempDir(t)
	snap := filepath.Join(root, "snap")
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(snap, link); err != nil {
		t.Fatal(err)
	}

	start := func(name, config string, annotations map[string]string) error {
		c := ns.Create(framework.UniqueID(name), framework.Spec{
			Args: []string{"sh", "-c", counter}, Annotations: annotations, RuntimeConfig: config,
		})
		return c.Start()
	}
	refused := func(what string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error saying %q", what, err, want)
		}
	}
	fork := map[string]string{container.AnnotationForkFrom: snap}
	allowed := fmt.Sprintf("PodAnnotations = [%q]\n", container.AnnotationForkFrom)
	withRoots := allowed + fmt.Sprintf("SnapshotRoots = [%q]\n", root)

	refused("pod, default handler", start("pod-default", "", podWith(fork)), "not allowed for Kubernetes pods")
	refused("pod, fork-from listed, no roots", start("pod-noroots", allowed, podWith(fork)), "needs the handler's SnapshotRoots")
	refused("pod, through a symlink", start("pod-link", withRoots,
		podWith(map[string]string{container.AnnotationForkFrom: link})), "symlink")
	refused("pod, migrate-in", start("pod-migrate", "", podWith(map[string]string{
		migrate.AnnotationIn: "0.0.0.0:7799", migrate.AnnotationToken: "x"})), "not allowed for Kubernetes pods")
	if err := start("pod-ok", withRoots, podWith(fork)); err != nil {
		t.Fatalf("pod forking from a snapshot under SnapshotRoots: %v", err)
	}
}

// TestForkKeyTraversal: a snapshot's key file names a range key, which
// becomes a directory gcr writes and removes as root. One that climbs out
// is refused, and what it points at is left alone.
func TestForkKeyTraversal(t *testing.T) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	victim := filepath.Join(framework.TempDir(t), "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(framework.TempDir(t), "fake")
	for _, d := range []string{"images", "upper"} {
		if err := os.MkdirAll(filepath.Join(fake, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// From /run/gcr/userns/keys, up to / and down to the victim.
	if err := os.WriteFile(filepath.Join(fake, userns.KeyFile), []byte("../../../.."+victim), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, userns.BaseFile), []byte("1074790400"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := ns.Create(framework.UniqueID("traversal"), framework.Spec{
		Args:        []string{"sh", "-c", counter},
		Annotations: map[string]string{container.AnnotationForkFrom: fake},
	})
	err := c.Start()
	if err == nil || !strings.Contains(err.Error(), "user namespace range key") {
		t.Errorf("fork with a key climbing out: %v, want the key refused", err)
	}
	c.Remove()
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Fatalf("the directory the key pointed at was touched: %v", err)
	}
}
