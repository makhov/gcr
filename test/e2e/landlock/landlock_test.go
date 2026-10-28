//go:build e2e && linux

// Package landlock checks that a host directory descriptor reaching a
// container (the way runc's CVE-2024-21626 leaked one) leads nowhere from
// the container's processes, also after a checkpoint and restore, and that
// it does with Landlock = off. Processes started with exec stay outside,
// so they can still inspect the container's processes.
package landlock

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/userns"
	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

// serveFD hands a descriptor for dir to every connection on a unix socket
// in a new directory, which it returns.
func serveFD(t *testing.T, dir string) string {
	sockDir := framework.TempDir(t)
	if err := os.Chmod(sockDir, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sockDir, "fd.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	// The container's root is another user in its user namespace.
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			_, _, _ = c.WriteMsgUnix([]byte{0}, unix.UnixRights(fd), nil)
			c.Close()
		}
	}()
	return sockDir
}

type env struct {
	ns     *framework.Namespace
	mounts []specs.Mount
}

func setup(t *testing.T) *env {
	host := framework.TempDir(t)
	if err := os.WriteFile(filepath.Join(host, "secret"), []byte("host-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &env{
		ns: framework.NewNamespace(t, "gcr-e2e"),
		mounts: []specs.Mount{
			framework.BindRO(framework.BuildStatic(t, "leak"), "/leak"),
			{Type: "bind", Source: serveFD(t, host), Destination: "/sock", Options: []string{"rbind"}},
		},
	}
}

// run starts a container whose init's child tries the leaked descriptor
// every half second and writes the outcome to /tmp/result.
func (e *env) run(runtimeConfig string, annotations ...string) *framework.Container {
	a := map[string]string{}
	for i := 0; i+1 < len(annotations); i += 2 {
		a[annotations[i]] = annotations[i+1]
	}
	return e.ns.Run(framework.UniqueID("landlock"), framework.Spec{
		Args: []string{"sh", "-c",
			"while true; do /leak /sock/fd.sock >/tmp/result.new; mv /tmp/result.new /tmp/result; sleep 0.5; done"},
		Mounts:        e.mounts,
		RuntimeConfig: runtimeConfig,
		Annotations:   a,
	})
}

// result waits for a fresh outcome.
func result(t *testing.T, c *framework.Container) string {
	t.Helper()
	_ = os.Remove(c.Rootfs("/tmp/result"))
	var got string
	framework.Eventually(t, 10*time.Second, "a result", func() bool {
		got, _ = c.ReadFile("/tmp/result")
		return got != ""
	})
	return got
}

func TestLandlockOff(t *testing.T) {
	c := setup(t).run(`Landlock = "off"`)
	if got := result(t, c); got != "OK host-secret" {
		t.Fatalf("with Landlock off the leaked descriptor should reach the host: %q", got)
	}
}

func TestLandlock(t *testing.T) {
	c := setup(t).run("")
	if got := result(t, c); got != "EACCES" {
		t.Fatalf("confined container: %q", got)
	}

	// exec stays outside, so it can inspect the container's processes.
	if out, code := c.Exec("cat", "/proc/1/environ"); code != 0 || !strings.Contains(out, "PATH=") {
		t.Fatalf("exec cannot read /proc/1/environ (exit %d): %s", code, out)
	}

	// The kernel cannot report a Landlock domain for CRIU to dump; gcr
	// has the restore confine the container again.
	c.CheckpointRestore()
	if got := result(t, c); got != "EACCES" {
		t.Fatalf("restored container: %q", got)
	}
}

// TestDebugContainer starts a container in another's PID namespace, as
// kubectl debug --target does: it is left out of Landlock, so it can
// inspect the confined container's processes.
func TestDebugContainer(t *testing.T) {
	// Host users for both: in two user namespaces of their own, the debug
	// container could not inspect the app whatever Landlock did.
	e := setup(t)
	app := e.run("", userns.AnnotationHostUsers, "true")
	if got := result(t, app); got != "EACCES" {
		t.Fatalf("app not confined: %q", got)
	}
	out, code := e.ns.Output(framework.UniqueID("debug"), framework.Spec{
		Annotations: map[string]string{userns.AnnotationHostUsers: "true"},
		Args: []string{"sh", "-c", "tr '\\0' '\\n' </proc/1/environ | grep -c PATH="},
		SpecOpts: []oci.SpecOpts{oci.WithLinuxNamespace(specs.LinuxNamespace{
			Type: specs.PIDNamespace, Path: fmt.Sprintf("/proc/%d/ns/pid", app.Task.Pid()),
		})},
	})
	if code != 0 {
		t.Fatalf("debug container cannot read the app's /proc/1/environ (exit %d): %s", code, out)
	}
}
