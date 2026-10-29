#!/bin/bash
# Installs CRI-O next to containerd, with gcr as runtime handler "gcr"
# (runtime type "vm": CRI-O starts containerd shims and drives them
# through the task API), and starts it on /run/crio/crio.sock. Its
# binaries go to /opt/crio and its storage to /var/lib/crio-storage, so
# containerd's own crictl config, CNI plugins and images are untouched.
# Uses the CNI config hack/ci-setup.sh writes. Run as root.
set -euxo pipefail

CRIO_VERSION=${CRIO_VERSION:-v1.37.2}
ARCH=$(dpkg --print-architecture)

tmp=$(mktemp -d)
curl -fsSL "https://storage.googleapis.com/cri-o/artifacts/cri-o.${ARCH}.${CRIO_VERSION}.tar.gz" | tar -xz -C "$tmp"
mkdir -p /opt/crio/bin
cp "$tmp"/cri-o/bin/{crio,pinns,conmon,crun,crictl} /opt/crio/bin/
mkdir -p /etc/containers
[ -e /etc/containers/policy.json ] ||
	echo '{"default":[{"type":"insecureAcceptAnything"}]}' >/etc/containers/policy.json

cat >/opt/crio/crio.conf <<CONF
[crio]
root = "/var/lib/crio-storage"
runroot = "/run/crio-storage"
[crio.api]
listen = "/run/crio/crio.sock"
[crio.runtime]
default_runtime = "crun"
pinns_path = "/opt/crio/bin/pinns"
[crio.runtime.runtimes.crun]
runtime_path = "/opt/crio/bin/crun"
monitor_path = "/opt/crio/bin/conmon"
[crio.runtime.runtimes.gcr]
runtime_path = "$(command -v containerd-shim-gcr-v2)"
runtime_type = "vm"
runtime_root = "/run/gcr-crio"
[crio.network]
network_dir = "/etc/cni/net.d/"
plugin_dirs = ["/opt/cni/bin/"]
CONF

systemctl stop crio-gcr 2>/dev/null || true
systemctl reset-failed crio-gcr 2>/dev/null || true
systemd-run --unit crio-gcr /opt/crio/bin/crio --config /opt/crio/crio.conf
for _ in $(seq 50); do
	/opt/crio/bin/crictl -r unix:///run/crio/crio.sock version && break
	sleep 0.2
done
