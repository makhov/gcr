#!/bin/bash
# Prepares a Linux host (CI runner or VM) to run gcr's end-to-end tests:
# containerd with the gcr runtime, CNI plugins, crictl/critest, and the CRIU
# fork with --seccomp-notify-socket. Run as root.
set -euxo pipefail

# What CRIU's restores depend on, for reading a failed run.
uname -a
grep -H . /sys/kernel/mm/transparent_hugepage/enabled /sys/kernel/mm/transparent_hugepage/defrag \
	/proc/sys/vm/unprivileged_userfaultfd 2>/dev/null || true

CONTAINERD_VERSION=${CONTAINERD_VERSION:-2.4.2}
CNI_VERSION=${CNI_VERSION:-v1.9.1}
CRI_TOOLS_VERSION=${CRI_TOOLS_VERSION:-v1.37.0}
CRIU_REPO=${CRIU_REPO:-https://github.com/makhov/criu.git}
# gcr's CRIU: the makhov/criu tag in hack/criu-version, from its gcr branch.
CRIU_REF=${CRIU_REF:-$(cat "$(dirname "$0")/criu-version")}
ARCH=$(dpkg --print-architecture)
export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y build-essential pkg-config git jq libseccomp-dev \
	libprotobuf-dev libprotobuf-c-dev protobuf-c-compiler protobuf-compiler python3-protobuf \
	libnl-3-dev libnl-route-3-dev libcap-dev libnet1-dev libaio-dev libgnutls28-dev \
	libbsd-dev libdrm-dev uuid-dev libnftables-dev

# A distribution containerd (e.g. from Docker) would shadow ours.
systemctl stop docker.socket docker containerd 2>/dev/null || true

curl -fsSL "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${ARCH}.tar.gz" |
	tar -xz -C /usr/local
curl -fsSL -o /etc/systemd/system/containerd.service \
	"https://raw.githubusercontent.com/containerd/containerd/v${CONTAINERD_VERSION}/containerd.service"
mkdir -p /etc/containerd
case $CONTAINERD_VERSION in
1.*)
	# Config version 2; listing runtimes replaces the default runc entry.
	cat >/etc/containerd/config.toml <<'EOF'
version = 2
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.gcr]
  runtime_type = "io.containerd.gcr.v2"
  pod_annotations = ["gcr.dev/*"]
EOF
	;;
*)
	cat >/etc/containerd/config.toml <<'EOF'
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]
  runtime_type = 'io.containerd.gcr.v2'
  pod_annotations = ['gcr.dev/*']
EOF
	;;
esac

mkdir -p /opt/cni/bin /etc/cni/net.d
curl -fsSL "https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-linux-${ARCH}-${CNI_VERSION}.tgz" |
	tar -xz -C /opt/cni/bin
cat >/etc/cni/net.d/10-bridge.conflist <<'EOF'
{"cniVersion": "1.0.0", "name": "bridge", "plugins": [
  {"type": "bridge", "bridge": "cni0", "isGateway": true, "ipMasq": true,
   "ipam": {"type": "host-local", "ranges": [[{"subnet": "10.88.0.0/16"}]], "routes": [{"dst": "0.0.0.0/0"}]}},
  {"type": "portmap", "capabilities": {"portMappings": true}},
  {"type": "loopback"}]}
EOF
# Docker leaves the FORWARD policy at DROP, which cuts pods off from the
# outside (the Docker-in-Docker test pulls images from a pod).
iptables -P FORWARD ACCEPT
ip6tables -P FORWARD ACCEPT 2>/dev/null || true

for t in crictl critest; do
	curl -fsSL "https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRI_TOOLS_VERSION}/$t-${CRI_TOOLS_VERSION}-linux-${ARCH}.tar.gz" |
		tar -xz -C /usr/local/bin
done
printf 'runtime-endpoint: unix:///run/containerd/containerd.sock\nimage-endpoint: unix:///run/containerd/containerd.sock\n' >/etc/crictl.yaml

src=$(mktemp -d)
git clone --depth 1 --branch "$CRIU_REF" "$CRIU_REPO" "$src"
make -C "$src" -j"$(nproc)" criu cuda_plugin
install -m 0755 "$src/criu/criu" /usr/local/sbin/criu
# CRIU loads plugins from here; the CUDA plugin stays idle without a GPU.
install -D -m 0755 "$src/plugins/cuda/cuda_plugin.so" /usr/lib/criu/cuda_plugin.so
criu check || true

systemctl daemon-reload
systemctl enable --now containerd
