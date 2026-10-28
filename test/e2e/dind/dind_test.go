//go:build e2e && linux

// Package dind runs Docker-in-Docker in a gcr pod with a user namespace,
// without privileged: dockerd with CAP_SYS_ADMIN and CAP_NET_ADMIN (scoped
// to the pod's user and network namespaces), RuntimeDefault seccomp plus
// gcr's hardening and supervised mounts, and a delegated cgroup. It runs
// and builds nested containers.
//
// The pod gets its user namespace from CRI, as with Kubernetes hostUsers:
// false: containerd then creates the pod's network namespace inside it,
// which dockerd needs to manage (a network namespace the host owns cannot
// be configured from a user namespace).
package dind

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	cri "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

// base is the pod's first host ID (what kubelet would pick for hostUsers:
// false).
const base = 1879048192

func usernsOptions() *cri.UserNamespace {
	m := []*cri.IDMapping{{HostId: base, ContainerId: 0, Length: 65536}}
	return &cri.UserNamespace{Mode: cri.NamespaceMode_POD, Uids: m, Gids: m}
}

func TestDockerInDocker(t *testing.T) {
	c := framework.NewCRI(t)
	// dockerd's state; kubelet would make a volume the pod's root can use.
	state := t.TempDir()
	if err := os.Chown(state, base, base); err != nil {
		t.Fatal(err)
	}

	pod := c.RunPod(&cri.PodSandboxConfig{
		Metadata:  &cri.PodSandboxMetadata{Name: "dind"},
		DnsConfig: &cri.DNSConfig{Servers: []string{"1.1.1.1", "8.8.8.8"}},
		Linux: &cri.LinuxPodSandboxConfig{SecurityContext: &cri.LinuxSandboxSecurityContext{
			NamespaceOptions: &cri.NamespaceOption{UsernsOptions: usernsOptions()},
		}},
	})
	// --iptables=false: no NAT, so nested containers that need the outside
	// use --network host; Docker's default bridge (docker0 and a veth pair
	// per container, link kinds gcr's netlink policy allows) still joins
	// them to each other. AppArmor unconfined, as for privileged DinD: the
	// nested runtime's processes otherwise end up under labels the default
	// profile does not let signal each other.
	dockerd := &cri.ContainerConfig{
		Metadata: &cri.ContainerMetadata{Name: "dockerd"},
		Image:    &cri.ImageSpec{Image: "docker.io/library/docker:dind"},
		Command:  []string{"dockerd-entrypoint.sh"},
		Args:     []string{"dockerd", "--iptables=false", "--ip6tables=false", "--host=unix:///var/run/docker.sock"},
		Envs:     []*cri.KeyValue{{Key: "DOCKER_TLS_CERTDIR", Value: []byte{}}},
		Mounts:   []*cri.Mount{{ContainerPath: "/var/lib/docker", HostPath: state}},
		LogPath:  "dockerd.log",
		Linux: &cri.LinuxContainerConfig{SecurityContext: &cri.LinuxContainerSecurityContext{
			Capabilities: &cri.Capability{AddCapabilities: []string{"SYS_ADMIN", "NET_ADMIN"}},
			Seccomp:      &cri.SecurityProfile{ProfileType: cri.SecurityProfile_RuntimeDefault},
			Apparmor:     &cri.SecurityProfile{ProfileType: cri.SecurityProfile_Unconfined},
			NamespaceOptions: &cri.NamespaceOption{
				Network: cri.NamespaceMode_POD, Pid: cri.NamespaceMode_CONTAINER, UsernsOptions: usernsOptions(),
			},
		}},
	}
	id := pod.Start(dockerd)
	defer func() {
		if t.Failed() {
			t.Logf("dockerd log:\n%s", tail(pod.Log(dockerd), 30))
		}
	}()

	sh := func(script string) (string, int) { return c.Exec(id, 5*time.Minute, "sh", "-c", script) }
	framework.Eventually(t, time.Minute, "dockerd", func() bool {
		_, code := c.Exec(id, 10*time.Second, "docker", "info")
		return code == 0
	})
	info, _ := sh(`docker info --format "{{.Driver}} {{.CgroupDriver}}/{{.CgroupVersion}} {{.SecurityOptions}}"`)
	t.Logf("dockerd: %s", strings.TrimSpace(info))
	uidmap, _ := sh("cat /proc/self/uid_map")
	if want := fmt.Sprintf("0 %d 65536", base); strings.Join(strings.Fields(uidmap), " ") != want {
		t.Fatalf("dockerd not in the pod's user namespace: %q", uidmap)
	}

	t.Run("run", func(t *testing.T) {
		out, code := sh("docker run --rm --network host busybox echo hello from nested docker")
		if code != 0 || lastLine(out) != "hello from nested docker" {
			t.Fatalf("docker run (exit %d): %s", code, out)
		}
		nested, _ := sh("docker run --rm --network host busybox cat /proc/self/uid_map")
		if strings.Join(strings.Fields(lastLine(nested)), " ") != strings.Join(strings.Fields(uidmap), " ") {
			t.Fatalf("nested container's uid_map: %q", nested)
		}
	})
	t.Run("build", func(t *testing.T) {
		out, code := sh(`printf "FROM busybox\nRUN echo built inside > /built\nCMD cat /built\n" |
			docker build -q --network host -t nested - >/dev/null && docker run --rm --network host nested`)
		if code != 0 || lastLine(out) != "built inside" {
			t.Fatalf("docker build (exit %d): %s", code, out)
		}
	})
	// Docker's bridge network: dockerd made docker0 and makes a veth pair
	// per container, and two containers on it reach each other.
	t.Run("bridge network", func(t *testing.T) {
		out, code := sh(`docker run -d --name srv busybox sh -c "nc -l -p 8080 -e echo hi" >/dev/null &&
			ip=$(docker inspect -f "{{.NetworkSettings.IPAddress}}" srv) && echo "srv $ip" &&
			docker run --rm busybox nc -w 3 $ip 8080; rc=$?; docker rm -f srv >/dev/null 2>&1; exit $rc`)
		if code != 0 || lastLine(out) != "hi" {
			t.Fatalf("over the bridge (exit %d): %s", code, out)
		}
		t.Logf("%s", strings.TrimSpace(out))
	})
	// gcr's filter holds for nested containers without one of their own.
	t.Run("nested filter", func(t *testing.T) {
		out, _ := sh(`docker run --rm --network host --cap-add SYS_ADMIN --security-opt seccomp=unconfined \
			--security-opt apparmor=unconfined busybox sh -c "mkdir -p /mnt
			mount -t ramfs r /mnt 2>/dev/null && echo ramfs mounted || echo ramfs refused
			mount -t tmpfs t /mnt && echo tmpfs mounted"`)
		if !strings.Contains(out, "ramfs refused") || !strings.Contains(out, "tmpfs mounted") {
			t.Fatalf("nested mounts: %s", out)
		}
	})
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func tail(s string, n int) string {
	lines := strings.Split(s, "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
