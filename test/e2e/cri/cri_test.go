//go:build e2e && linux

// Package cri is a quick check of the gcr runtime handler through CRI: a
// pod in gcr's user namespace, exec, exit codes, logs, resource updates,
// stats, and what the container's root may do. critest covers the rest
// of CRI (terminals included).
package cri

import (
	"strings"
	"testing"
	"time"

	cri "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/makhov/gcr/internal/userns"
	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

func TestPod(t *testing.T) {
	c := framework.NewCRI(t)
	pod := c.RunPod(&cri.PodSandboxConfig{
		Metadata:    &cri.PodSandboxMetadata{Name: "smoke"},
		Annotations: map[string]string{userns.AnnotationHostUsers: "false"},
	})
	app := &cri.ContainerConfig{
		Metadata: &cri.ContainerMetadata{Name: "app"},
		Image:    &cri.ImageSpec{Image: framework.Busybox},
		Command:  []string{"sh", "-c", "echo hello from gcr; sleep 3600"},
		LogPath:  "app.log",
	}
	id := pod.Start(app)
	exec := func(cmd ...string) (string, int) { return c.Exec(id, 30*time.Second, cmd...) }
	run := func(t *testing.T, cmd ...string) string {
		t.Helper()
		out, code := exec(cmd...)
		if code != 0 {
			t.Fatalf("%v exited %d: %s", cmd, code, out)
		}
		return strings.TrimSpace(out)
	}

	t.Run("exec", func(t *testing.T) {
		if out := run(t, "sh", "-c", "echo $((6*7))"); out != "42" {
			t.Fatalf("exec output %q", out)
		}
		if _, code := exec("sh", "-c", "exit 3"); code != 3 {
			t.Fatalf("exit code %d, want 3", code)
		}
	})
	t.Run("logs", func(t *testing.T) {
		framework.Eventually(t, 5*time.Second, "the log line", func() bool {
			return strings.Contains(pod.Log(app), "hello from gcr")
		})
	})
	t.Run("update", func(t *testing.T) {
		_, err := c.Runtime.UpdateContainerResources(c.Ctx, &cri.UpdateContainerResourcesRequest{
			ContainerId: id, Linux: &cri.LinuxContainerResources{MemoryLimitInBytes: 64 << 20}})
		if err != nil {
			t.Fatal(err)
		}
		if got := run(t, "cat", "/sys/fs/cgroup/memory.max"); got != "67108864" {
			t.Fatalf("memory.max after update: %s", got)
		}
	})
	t.Run("stats", func(t *testing.T) {
		resp, err := c.Runtime.ContainerStats(c.Ctx, &cri.ContainerStatsRequest{ContainerId: id})
		if err != nil {
			t.Fatal(err)
		}
		if s := resp.Stats; s.GetCpu().GetUsageCoreNanoSeconds().GetValue() == 0 || s.GetMemory().GetWorkingSetBytes().GetValue() == 0 {
			t.Fatalf("empty stats: %v", s)
		}
	})
	t.Run("user namespace", func(t *testing.T) {
		f := strings.Fields(run(t, "cat", "/proc/self/uid_map"))
		if len(f) != 3 || f[0] != "0" || !strings.HasPrefix(f[1], "1073") || f[2] != "65536" {
			t.Fatalf("not in a gcr user namespace: %v", f)
		}
		if owner := run(t, "stat", "-c", "%u:%g", "/bin/sh"); owner != "0:0" {
			t.Fatalf("image files owned by %s inside", owner)
		}
		run(t, "sh", "-c", "echo ok > /written && chown 1000 /written")
		run(t, "sh", "-c", `nc -l -p 80 -w 1 </dev/null & sleep 0.3; netstat -ltn | grep -q ":80 "`)
	})
}
