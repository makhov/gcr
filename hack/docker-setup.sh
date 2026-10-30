#!/bin/bash
# Installs Docker's static binaries (dockerd and the CLI) next to
# containerd, with gcr as runtime "gcr", and starts dockerd on the host's
# containerd rather than one of its own. Run as root.
set -euxo pipefail

DOCKER_VERSION=${DOCKER_VERSION:-29.9.2}
case $(uname -m) in
aarch64) arch=aarch64 ;;
*) arch=x86_64 ;;
esac

tmp=$(mktemp -d)
curl -fsSL "https://download.docker.com/linux/static/stable/${arch}/docker-${DOCKER_VERSION}.tgz" | tar -xz -C "$tmp"
install -m 0755 "$tmp"/docker/{dockerd,docker,docker-init,docker-proxy} /usr/local/bin/

mkdir -p /etc/docker
# ip-forward-no-drop: leave the host's FORWARD policy alone; Docker's DROP
# cuts off the CNI bridges containerd's pods use.
cat >/etc/docker/daemon.json <<CONF
{
  "runtimes": {"gcr": {"runtimeType": "io.containerd.gcr.v2"}},
  "ip-forward-no-drop": true
}
CONF

systemctl stop docker-gcr 2>/dev/null || true
systemctl reset-failed docker-gcr 2>/dev/null || true
systemd-run --unit docker-gcr /usr/local/bin/dockerd --containerd=/run/containerd/containerd.sock
for _ in $(seq 50); do
	docker version && break
	sleep 0.2
done
