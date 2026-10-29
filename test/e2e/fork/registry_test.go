//go:build e2e && linux

package fork

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/snapshot"
	"github.com/makhov/gcr/internal/snapshot/registry"
	"github.com/makhov/gcr/test/e2e/framework"
)

// TestRegistry moves a snapshot of a fork through an OCI registry: it is
// pushed with the layer it was forked from, everything here is removed
// (containers, snapshots, and with them the user namespace range), and a
// fork of the pulled snapshot carries on from it.
func TestRegistry(t *testing.T) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	addr := "127.0.0.1:" + strconv.Itoa(5100+os.Getpid()%400)
	ns.Run(framework.UniqueID("registry"), framework.Spec{
		Image: "docker.io/library/registry:3",
		SpecOpts: []oci.SpecOpts{
			oci.WithHostNamespace(specs.NetworkNamespace),
			oci.WithEnv([]string{"REGISTRY_HTTP_ADDR=" + addr, "OTEL_TRACES_EXPORTER=none"}),
		},
	})
	framework.Eventually(t, time.Minute, "the registry to serve", func() bool {
		resp, err := http.Get("http://" + addr + "/v2/")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	spec := framework.Spec{Args: []string{"sh", "-c", counter}}
	parent := ns.Run(framework.UniqueID("parent"), spec)
	framework.Eventually(t, time.Minute, "the parent to count", func() bool {
		s, _ := parent.ReadFile("/tmp/count")
		n, err := strconv.Atoi(s)
		return err == nil && n > 0
	})
	tmp := framework.TempDir(t)
	first := filepath.Join(tmp, "first")
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), first); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	forkOf := func(name, dir string) *framework.Container {
		s := spec
		s.Annotations = map[string]string{container.AnnotationForkFrom: dir}
		return ns.Run(framework.UniqueID(name), s)
	}
	a := forkOf("fork-a", first)
	if out, code := a.Exec("sh", "-c", "rm /parent && echo a > /a"); code != 0 {
		t.Fatalf("fork a: %s", out)
	}
	second := filepath.Join(tmp, "second")
	if err := control.NewClient(ns.Name, a.ID).Snapshot(t.Context(), second); err != nil {
		t.Fatalf("snapshot of a fork: %v", err)
	}
	at := a.ReadInt("/tmp/count")

	ref := fmt.Sprintf("%s/gcr-e2e/second:%d", addr, os.Getpid())
	repo, err := registry.Repository(ref, false)
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	if _, err := registry.Push(t.Context(), second, repo, repo.Reference.Reference); err != nil {
		t.Fatalf("push: %v", err)
	}
	t.Logf("pushed in %s", time.Since(begin).Round(time.Millisecond))

	// Nothing of it is left here.
	a.Remove()
	parent.Remove()
	for _, d := range []string{second, first} {
		if err := snapshot.Remove(d); err != nil {
			t.Fatalf("remove %s: %v", d, err)
		}
	}

	pulled := filepath.Join(tmp, "pulled")
	store := filepath.Join(tmp, "layers")
	begin = time.Now()
	if err := registry.Pull(t.Context(), repo, repo.Reference.Reference, pulled, store); err != nil {
		t.Fatalf("pull: %v", err)
	}
	t.Logf("pulled in %s", time.Since(begin).Round(time.Millisecond))

	b := forkOf("fork-b", pulled)
	if s, err := b.ReadFile("/a"); err != nil || s != "a" {
		t.Fatalf("fork of the pulled snapshot: /a is %q, %v", s, err)
	}
	if out, code := b.Exec("test", "-e", "/parent"); code == 0 {
		t.Fatalf("fork of the pulled snapshot sees /parent, which fork a deleted: %s", out)
	}
	framework.Eventually(t, 10*time.Second, "the fork to count on", func() bool {
		return b.ReadInt("/tmp/count") > at
	})
	if users, err := snapshot.Users(pulled); err != nil || len(users) != 1 {
		t.Fatalf("users of the pulled snapshot: %v, %v; want the fork", users, err)
	}
	b.Remove()
	if err := snapshot.Remove(pulled); err != nil {
		t.Fatalf("remove the pulled snapshot: %v", err)
	}
	layers, _ := os.ReadDir(store)
	for _, l := range layers {
		if err := snapshot.Remove(filepath.Join(store, l.Name())); err != nil {
			t.Fatalf("remove pulled layer: %v", err)
		}
	}
}
