//go:build e2e && linux

// Package crio runs gcr under CRI-O, as a runtime of type "vm": CRI-O
// starts the shim the way containerd before 2.3 did and drives it through
// the task API's version 2. A pod and a container run, exec and logs
// work, the container's processes are the shim's, and stopping and
// removing them takes the shim with it. Skipped without CRI-O
// (/run/crio/crio.sock, with gcr as runtime handler "gcr").
package crio

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	cri "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/makhov/gcr/test/e2e/framework"
)

const socket = "/run/crio/crio.sock"

func TestMain(m *testing.M) { framework.Main(m) }

func TestPod(t *testing.T) {
	if _, err := os.Stat(socket); err != nil {
		t.Skip("no CRI-O")
	}
	c := framework.NewCRIAt(t, socket)
	pod := c.RunPod(&cri.PodSandboxConfig{Metadata: &cri.PodSandboxMetadata{Name: "crio"}})
	app := &cri.ContainerConfig{
		Metadata: &cri.ContainerMetadata{Name: "app"},
		Image:    &cri.ImageSpec{Image: framework.Busybox},
		Command:  []string{"sh", "-c", "echo hello from gcr; sleep 3600"},
		LogPath:  "app.log",
	}
	id := pod.Start(app)

	if out, code := c.Exec(id, 30*time.Second, "sh", "-c", "echo $((6*7))"); code != 0 || strings.TrimSpace(out) != "42" {
		t.Fatalf("exec: %q, exit %d", out, code)
	}
	if _, code := c.Exec(id, 30*time.Second, "sh", "-c", "exit 3"); code != 3 {
		t.Fatalf("exit code %d, want 3", code)
	}
	framework.Eventually(t, 5*time.Second, "the log line", func() bool {
		return strings.Contains(pod.Log(app), "hello from gcr")
	})

	// CRI-O ran it through gcr, not its default runtime: the container's
	// init is a child of gcr's shim.
	st, err := c.Runtime.ContainerStatus(c.Ctx, &cri.ContainerStatusRequest{ContainerId: id, Verbose: true})
	if err != nil {
		t.Fatal(err)
	}
	pid := initPid(t, st.Info["info"])
	ppid := parentOf(t, pid)
	if exe, _ := os.Readlink("/proc/" + strconv.Itoa(ppid) + "/exe"); !strings.HasSuffix(exe, "containerd-shim-gcr-v2") {
		t.Fatalf("the container's init (%d) is a child of %s, not gcr's shim", pid, exe)
	}

	if _, err := c.Runtime.StopContainer(c.Ctx, &cri.StopContainerRequest{ContainerId: id, Timeout: 10}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	st, err = c.Runtime.ContainerStatus(c.Ctx, &cri.ContainerStatusRequest{ContainerId: id})
	if err != nil || st.Status.State != cri.ContainerState_CONTAINER_EXITED {
		t.Fatalf("after stop: %v, %v", st.GetStatus().GetState(), err)
	}
	if _, err := c.Runtime.RemoveContainer(c.Ctx, &cri.RemoveContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	framework.Eventually(t, 10*time.Second, "the container's shim to exit", func() bool {
		_, err := os.Stat("/proc/" + strconv.Itoa(ppid))
		return os.IsNotExist(err)
	})
}

// initPid reads the pid from CRI-O's verbose container info.
func initPid(t *testing.T, info string) int {
	t.Helper()
	_, after, ok := strings.Cut(info, `"pid":`)
	if !ok {
		t.Fatalf("no pid in %q", info)
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.FieldsFunc(after, func(r rune) bool { return r == ',' || r == '}' })[0]))
	if err != nil {
		t.Fatalf("pid in %q: %v", info, err)
	}
	return n
}

func parentOf(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	// After the command name, which may hold spaces: state, then ppid.
	fields := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatal(err)
	}
	return ppid
}
