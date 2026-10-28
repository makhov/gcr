//go:build e2e && linux

// Package hardening runs probe (a static program, built for the test) in
// containers with Hardening = standard (the default) and off, and checks
// what gets refused; and checks the mounts gcr's supervisor performs for
// containers that can mount.
package hardening

import (
	"os"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/makhov/gcr/internal/userns"
	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

// results parses "name result" lines.
func results(out string) map[string]string {
	r := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			r[f[0]] = f[1]
		}
	}
	return r
}

func expect(t *testing.T, got map[string]string, want string, names ...string) {
	t.Helper()
	for _, n := range names {
		if got[n] != want {
			t.Errorf("%s: got %q, want %s", n, got[n], want)
		}
	}
}

type env struct {
	ns    *framework.Namespace
	probe string
}

func setup(t *testing.T) *env {
	return &env{ns: framework.NewNamespace(t, "gcr-e2e"), probe: framework.BuildStatic(t, "probe")}
}

// run runs the probe with args in a container and parses its output.
func (e *env) run(t *testing.T, s framework.Spec, args ...string) map[string]string {
	t.Helper()
	s.Args = append([]string{"/probe"}, args...)
	s.Mounts = append(s.Mounts, framework.BindRO(e.probe, "/probe"))
	out, code := e.ns.Output(framework.UniqueID("hard"), s)
	t.Logf("probe %v:\n%s", args, out)
	if code != 0 {
		t.Fatalf("probe exited %d", code)
	}
	return results(out)
}

var withSysAdmin = []oci.SpecOpts{oci.WithAddedCapabilities([]string{"CAP_SYS_ADMIN"})}

func TestRefused(t *testing.T) {
	e := setup(t)
	got := e.run(t, framework.Spec{})
	expect(t, got, "EPERM", "io_uring_setup", "bpf", "perf_event_open", "userfaultfd", "fsopen",
		"open_tree", "init_module", "open_by_handle_at",
		"socket_packet", "socket_raw", "socket_vsock", "socket_alg")
	expect(t, got, "ENOSYS", "clone3", "keyctl")
	expect(t, got, "OK", "control_socket_tcp", "control_socket_ping", "control_fork")
}

func TestOff(t *testing.T) {
	e := setup(t)
	got := e.run(t, framework.Spec{RuntimeConfig: `Hardening = "off"`})
	for _, c := range []string{"io_uring_setup", "socket_alg", "clone3"} {
		if got[c] == "EPERM" || got[c] == "ENOSYS" {
			t.Errorf("%s still refused with Hardening = off (%s)", c, got[c])
		}
	}
}

func TestSupervisedMount(t *testing.T) {
	e := setup(t)
	for _, tc := range []struct {
		name        string
		annotations map[string]string
	}{
		{"user namespace", nil},
		{"host users", map[string]string{userns.AnnotationHostUsers: "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := e.run(t, framework.Spec{SpecOpts: withSysAdmin, Annotations: tc.annotations}, "mount")
			// A refusal is -1 with errno set, not a value a caller would
			// take for success.
			expect(t, got, "EPERM", "mount_ramfs")
			expect(t, got, "OK", "mount_tmpfs", "mount_self_task_ns", "mount_self_task_ns_same",
				"mount_thread_self_ns", "mount_thread_self_ns_same", "mount_fd_target", "mount_fd_target_same")
		})
	}
}

// TestSupervisedMountShell mounts with busybox, which reads mount options
// back from /proc/mounts for a remount.
func TestSupervisedMountShell(t *testing.T) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	script := `
		try() { name=$1; shift; if "$@" 2>/dev/null; then echo "$name OK"; else echo "$name EPERM"; fi; }
		mkdir -p /mnt /opt /media
		try tmpfs mount -t tmpfs none /mnt
		grep -q " /mnt tmpfs " /proc/mounts && echo "visible OK" || echo "visible MISSING"
		try proc mount -t proc proc /media
		mkdir -p /srv/a /srv/b; try bind mount --bind /srv/a /srv/b
		# A bind remount: a plain remount would pass the options from
		# /proc/mounts, where tmpfs shows its owner as an unmapped host ID.
		try remount mount -o remount,bind,ro /mnt`
	out, _ := ns.Output(framework.UniqueID("mnt"), framework.Spec{Args: []string{"sh", "-c", script}, SpecOpts: withSysAdmin})
	t.Logf("\n%s", out)
	expect(t, results(out), "OK", "tmpfs", "visible", "proc", "bind", "remount")

	// Without CAP_SYS_ADMIN the kernel refuses, and gcr does not mount for
	// the container.
	out, _ = ns.Output(framework.UniqueID("mnt"), framework.Spec{Args: []string{"sh", "-c", script}})
	expect(t, results(out), "EPERM", "tmpfs")
}

// withoutHostUsernsRestriction turns off Ubuntu's AppArmor restriction on
// unprivileged user namespaces for the test: it moves a process that makes
// one to a profile without capabilities, which would decide the outcome
// instead of gcr (on runc as much: Chrome needs a profile of its own there).
func withoutHostUsernsRestriction(t *testing.T) {
	const knob = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
	old, err := os.ReadFile(knob)
	if err != nil || strings.TrimSpace(string(old)) == "0" {
		return
	}
	if err := os.WriteFile(knob, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(knob, old, 0o644) })
}

func TestNestedUserNamespaces(t *testing.T) {
	withoutHostUsernsRestriction(t)
	e := setup(t)
	t.Run("guarded", func(t *testing.T) {
		got := e.run(t, framework.Spec{}, "nested")
		expect(t, got, "OK", "nested_userns", "nested_netlink_route")
		expect(t, got, "EPERM", "nested_netfilter", "nested_mount_ramfs",
			// Landlock refuses a confined container any mount.
			"nested_mount_tmpfs")
	})
	t.Run("guarded without Landlock", func(t *testing.T) {
		got := e.run(t, framework.Spec{RuntimeConfig: `Landlock = "off"`}, "nested")
		expect(t, got, "OK", "nested_userns", "nested_mount_tmpfs")
		// The supervisor's filesystem allowlist holds in the nested
		// namespaces.
		expect(t, got, "EPERM", "nested_netfilter", "nested_mount_ramfs")
	})
	// Control: without gcr's filter the same calls go through, so the
	// refusals above are gcr's.
	t.Run("hardening off", func(t *testing.T) {
		got := e.run(t, framework.Spec{RuntimeConfig: "Hardening = \"off\"\nLandlock = \"off\""}, "nested")
		expect(t, got, "OK", "nested_userns", "nested_netfilter", "nested_mount_ramfs", "nested_mount_tmpfs")
	})
	t.Run("deny", func(t *testing.T) {
		got := e.run(t, framework.Spec{RuntimeConfig: `NestedUserNamespaces = "deny"`}, "nested")
		expect(t, got, "EPERM", "nested_userns")
	})
}
