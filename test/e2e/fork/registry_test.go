//go:build e2e && linux

package fork

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/nydus"
	"github.com/makhov/gcr/internal/snapshot"
	"github.com/makhov/gcr/internal/snapshot/registry"
	"github.com/makhov/gcr/internal/userns"
	"github.com/makhov/gcr/test/e2e/framework"
)

// TestRegistry moves a snapshot of a fork through an OCI registry: it is
// pushed with the layer it was forked from, and pulled twice. First while
// the snapshots here hold its user namespace range, so it is moved into
// another, as on a host where that range is taken; then once everything
// here is removed, so it keeps its own. A fork of either carries on from
// it, mapping the range it was given.
func TestRegistry(t *testing.T) {
	t.Run("tar", func(t *testing.T) { testRegistry(t, registry.FormatTar) })
	t.Run("nydus", func(t *testing.T) {
		if err := nydus.Available(); err != nil {
			t.Skip(err)
		}
		testRegistry(t, registry.FormatNydus)
	})
}

func testRegistry(t *testing.T, format string) {
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

	ref := fmt.Sprintf("%s/gcr-e2e/second-%s:%d", addr, format, os.Getpid())
	repo, err := registry.Repository(ref, false)
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	if _, err := registry.Push(t.Context(), second, repo, repo.Reference.Reference, format); err != nil {
		t.Fatalf("push: %v", err)
	}
	t.Logf("pushed in %s", time.Since(begin).Round(time.Millisecond))

	base := readBase(t, second)
	store := filepath.Join(tmp, "layers")

	// Its range is held here: the pull moves it.
	moved := filepath.Join(tmp, "moved")
	res, err := registry.Pull(t.Context(), repo, repo.Reference.Reference, moved, store)
	if err != nil {
		t.Fatalf("pull into a taken range: %v", err)
	}
	if res.From != base || res.Range.Base == base || readBase(t, moved) != res.Range.Base {
		t.Fatalf("pulled while range %d is taken: %+v, snapshot says %d", base, res, readBase(t, moved))
	}
	t.Logf("moved from range %d to %d", base, res.Range.Base)
	fm := forkOf("fork-moved", moved)
	checkFork(t, fm, at, res.Range.Base)
	// Gone again, with the layer it moved into the store.
	fm.Remove()
	removeAll(t, moved, store)

	// Nothing of it is left here.
	a.Remove()
	parent.Remove()
	for _, d := range []string{second, first} {
		if err := snapshot.Remove(d); err != nil {
			t.Fatalf("remove %s: %v", d, err)
		}
	}

	pulled := filepath.Join(tmp, "pulled")
	begin = time.Now()
	res, err = registry.Pull(t.Context(), repo, repo.Reference.Reference, pulled, store)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	t.Logf("pulled in %s", time.Since(begin).Round(time.Millisecond))
	if res.From != 0 || res.Range.Base != base {
		t.Fatalf("pulled with range %d free: %+v", base, res)
	}
	b := forkOf("fork-b", pulled)
	checkFork(t, b, at, base)
	if users, err := snapshot.Users(pulled); err != nil || len(users) != 1 {
		t.Fatalf("users of the pulled snapshot: %v, %v; want the fork", users, err)
	}
}

// checkFork checks that c, a fork of the snapshot of fork a, has a's
// files and counter, and maps the range at base.
func checkFork(t *testing.T, c *framework.Container, at int, base uint32) {
	t.Helper()
	if s, err := c.ReadFile("/a"); err != nil || s != "a" {
		t.Fatalf("%s: /a is %q, %v", c.ID, s, err)
	}
	if out, code := c.Exec("test", "-e", "/parent"); code == 0 {
		t.Fatalf("%s sees /parent, which fork a deleted: %s", c.ID, out)
	}
	framework.Eventually(t, 10*time.Second, c.ID+" to count on", func() bool {
		return c.ReadInt("/tmp/count") > at
	})
	out, code := c.Exec("cat", "/proc/self/uid_map")
	if f := strings.Fields(out); code != 0 || len(f) < 2 || f[1] != strconv.Itoa(int(base)) {
		t.Fatalf("%s maps %q, want range %d", c.ID, out, base)
	}
	// What it writes is owned, on the host, by its range's root, as is
	// what it found in the snapshot's layer.
	if _, code := c.Exec("sh", "-c", "echo x > /new"); code != 0 {
		t.Fatalf("%s: write", c.ID)
	}
	for _, f := range []string{"/new", "/a"} {
		var st syscall.Stat_t
		if err := syscall.Stat(c.Rootfs(f), &st); err != nil || st.Uid != base {
			t.Fatalf("%s: %s is owned by %d on the host, want %d (%v)", c.ID, f, st.Uid, base, err)
		}
	}
}

// removeAll removes the snapshot in dir and the layers in store.
func removeAll(t *testing.T, dir, store string) {
	t.Helper()
	if err := snapshot.Remove(dir); err != nil {
		t.Fatalf("remove %s: %v", dir, err)
	}
	layers, _ := os.ReadDir(store)
	for _, l := range layers {
		if err := snapshot.Remove(filepath.Join(store, l.Name())); err != nil {
			t.Fatalf("remove layer %s: %v", l.Name(), err)
		}
	}
}

func readBase(t *testing.T, dir string) uint32 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, userns.BaseFile))
	if err != nil {
		t.Fatal(err)
	}
	n, err := userns.ParseBase(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
