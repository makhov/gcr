//go:build e2e && linux

package crio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	cri "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/snapshot"
	"github.com/makhov/gcr/internal/timetravel"
	"github.com/makhov/gcr/test/e2e/framework"
)

// forksHandler lets pods fork from snapshots under snapshotRoot
// (hack/crio-setup.sh).
const (
	forksHandler = "gcr-forks"
	snapshotRoot = "/var/lib/gcr/snapshots"
)

// TestFork: under CRI-O, which mounts the container's rootfs itself, a
// running container is snapshotted, forked into another pod, and the
// fork snapshotted and forked again. Each fork carries on from its
// snapshot's memory, writes only its own layer, and keeps the snapshots
// beneath it from being removed.
func TestFork(t *testing.T) {
	if _, err := os.Stat(socket); err != nil {
		t.Skip("no CRI-O")
	}
	c := framework.NewCRIAt(t, socket)
	counter := func(name string, annotations map[string]string) *cri.ContainerConfig {
		return &cri.ContainerConfig{
			Metadata:    &cri.ContainerMetadata{Name: name},
			Image:       &cri.ImageSpec{Image: framework.Busybox},
			Command:     []string{"sh", "-c", "trap 'exit 0' TERM; i=0; while :; do i=$((i+1)); echo $i > /count; sleep 0.1; done"},
			LogPath:     name + ".log",
			Annotations: annotations,
			// As PID 1 it would ignore SIGTERM, and every stop would wait
			// out its grace period. A pid namespace of its own, as kubelet
			// asks unless the pod shares one.
			Linux: &cri.LinuxContainerConfig{SecurityContext: &cri.LinuxContainerSecurityContext{
				NamespaceOptions: &cri.NamespaceOption{Pid: cri.NamespaceMode_CONTAINER}}},
		}
	}
	count := func(id string) int {
		t.Helper()
		out, code := c.Exec(id, 30*time.Second, "cat", "/count")
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if code != 0 || err != nil {
			t.Fatalf("count in %s: %q, exit %d", id, out, code)
		}
		return n
	}
	snap := func(id string) string {
		t.Helper()
		dir := filepath.Join(snapshotRoot, framework.UniqueID("crio"))
		if err := control.NewClient("default", id).Snapshot(c.Ctx, dir); err != nil {
			t.Fatalf("snapshot %s: %v", id, err)
		}
		t.Cleanup(func() { _ = snapshot.Remove(dir) })
		return dir
	}
	// fork runs a container from dir in a pod of its own, and checks it
	// carries on from the snapshot's count.
	fork := func(name, dir string) (string, *framework.Pod) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "upper", "count"))
		if err != nil {
			t.Fatal(err)
		}
		at, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		pod := c.RunPodWith(forksHandler, &cri.PodSandboxConfig{
			Metadata:    &cri.PodSandboxMetadata{Name: name},
			Annotations: map[string]string{container.AnnotationForkFrom: dir},
		})
		cfg := counter(name, nil)
		cfg.Command = []string{"true"} // not run: the snapshot's processes are
		id := pod.Start(cfg)
		first := count(id)
		if first < at {
			t.Fatalf("%s counts from %d, its snapshot was at %d", name, first, at)
		}
		framework.Eventually(t, 10*time.Second, name+" to keep counting", func() bool { return count(id) > first })
		return id, pod
	}

	parentPod := c.RunPod(&cri.PodSandboxConfig{Metadata: &cri.PodSandboxMetadata{Name: "parent"}})
	parent := parentPod.Start(counter("parent", nil))
	framework.Eventually(t, 10*time.Second, "the parent to count", func() bool { return count(parent) > 5 })
	dir := snap(parent)

	child, childPod := fork("child", dir)
	var info container.ForkInfo
	out, _ := c.Exec(child, 30*time.Second, "cat", container.ForkFile)
	if err := json.Unmarshal([]byte(out), &info); err != nil || info.ID != child || info.Snapshot != dir {
		t.Fatalf("%s in the fork: %q (%v)", container.ForkFile, out, err)
	}
	if _, code := c.Exec(child, 30*time.Second, "sh", "-c", "echo mine > /mine"); code != 0 {
		t.Fatal("the fork cannot write")
	}
	if _, code := c.Exec(parent, 30*time.Second, "test", "-e", "/mine"); code == 0 {
		t.Fatal("the fork's write reached the parent")
	}

	// A snapshot of the fork holds only what the fork changed, above the
	// parent's snapshot, and a fork of it mounts both.
	dir2 := snap(child)
	layers, err := container.ReadLayers(dir2)
	if err != nil || !slices.Contains(layers, filepath.Join(dir, snapshot.Upper)) {
		t.Fatalf("layers of a snapshot of the fork: %q (%v)", layers, err)
	}
	grandchild, grandchildPod := fork("grandchild", dir2)
	if out, code := c.Exec(grandchild, 30*time.Second, "cat", "/mine"); code != 0 || strings.TrimSpace(out) != "mine" {
		t.Fatalf("the fork of a fork lacks its snapshot's file: %q", out)
	}

	if err := snapshot.Remove(dir); err == nil {
		t.Fatal("removed a snapshot in use")
	}
	for _, p := range []*framework.Pod{grandchildPod, childPod} {
		if _, err := c.Runtime.StopPodSandbox(c.Ctx, &cri.StopPodSandboxRequest{PodSandboxId: p.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Runtime.RemovePodSandbox(c.Ctx, &cri.RemovePodSandboxRequest{PodSandboxId: p.ID}); err != nil {
			t.Fatalf("remove a fork's pod: %v", err)
		}
	}
	// Removing the forks let go of the snapshots, and of nothing more:
	// the parent runs on.
	for _, d := range []string{dir2, dir} {
		if err := snapshot.Remove(d); err != nil {
			t.Fatalf("remove %s after its forks: %v", d, err)
		}
	}
	count(parent)
}

// TestTimeTravelRefused: CRI-O tracks a container by its init's pid, which
// a rewind replaces, so gcr refuses time travel there and says why.
func TestTimeTravelRefused(t *testing.T) {
	if _, err := os.Stat(socket); err != nil {
		t.Skip("no CRI-O")
	}
	c := framework.NewCRIAt(t, socket)
	pod := c.RunPod(&cri.PodSandboxConfig{
		Metadata:    &cri.PodSandboxMetadata{Name: "timetravel"},
		Annotations: map[string]string{timetravel.AnnotationTrigger: "execve"},
	})
	c.Pull(framework.Busybox)
	_, err := c.Runtime.CreateContainer(c.Ctx, &cri.CreateContainerRequest{
		PodSandboxId: pod.ID, SandboxConfig: pod.Config,
		Config: &cri.ContainerConfig{
			Metadata: &cri.ContainerMetadata{Name: "tt"},
			Image:    &cri.ImageSpec{Image: framework.Busybox},
			Command:  []string{"sleep", "60"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "time travel in a container CRI-O runs") {
		t.Fatalf("time travel under CRI-O: %v", err)
	}
}
