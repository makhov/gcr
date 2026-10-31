//go:build e2e && linux

package hardening

import (
	"os"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/makhov/gcr/test/e2e/framework"
)

// TestSupervisedMountSELinux: the supervisor mounts as the container
// would, under its SELinux label too. Fedora's policy (container-selinux)
// refuses container_t a proc mount and a context= option, which the
// runtime's own domain may make; tmpfs it allows.
func TestSupervisedMountSELinux(t *testing.T) {
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err != nil {
		t.Skip("SELinux is not enabled")
	}
	if b, _ := os.ReadFile("/sys/fs/selinux/enforce"); string(b) != "1" {
		t.Skip("SELinux is not enforcing")
	}
	ns := framework.NewNamespace(t, "gcr-e2e")
	script := `
		try() { name=$1; shift; mkdir -p /m/$name; if "$@" /m/$name 2>/dev/null; then echo "$name OK"; else echo "$name EACCES"; fi; }
		echo "label $(cat /proc/self/attr/current | tr -d '\0')"
		try tmpfs mount -t tmpfs none
		try context mount -t tmpfs -o context=system_u:object_r:etc_t:s0 none
		try proc mount -t proc proc`
	out, _ := ns.Output(framework.UniqueID("mnt"), framework.Spec{
		Args:     []string{"sh", "-c", script},
		SpecOpts: append(withSysAdmin, oci.WithSelinuxLabel("system_u:system_r:container_t:s0:c7,c8")),
	})
	t.Logf("\n%s", out)
	got := results(out)
	if got["label"] != "system_u:system_r:container_t:s0:c7,c8" {
		t.Fatalf("the container runs as %q, not container_t: this host's policy cannot run the test", got["label"])
	}
	expect(t, got, "OK", "tmpfs")
	expect(t, got, "EACCES", "context", "proc")
}
