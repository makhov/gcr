#!/bin/bash
# Runs a command against another containerd release, in place of the
# installed one, then puts that back:
#
#	sudo hack/with-containerd.sh 1.7.29 make test-e2e E2E=./test/e2e/checkpoint/
#
# The release is downloaded into /opt/containerd-VERSION. It gets the
# same socket and a config of its own (containerd 1.x needs config
# version 2), with gcr registered for CRI.
set -euo pipefail

version=$1
shift
arch=$(dpkg --print-architecture)
dir=/opt/containerd-$version
if [ ! -x "$dir/bin/containerd" ]; then
	mkdir -p "$dir"
	curl -fsSL "https://github.com/containerd/containerd/releases/download/v${version}/containerd-${version}-linux-${arch}.tar.gz" |
		tar -xz -C "$dir"
fi

config=$dir/config.toml
case $version in
1.*)
	cat >"$config" <<'EOF'
version = 2
# Listing runtimes replaces the default runc entry in 1.x.
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.gcr]
  runtime_type = "io.containerd.gcr.v2"
  pod_annotations = ["gcr.dev/*"]
EOF
	;;
*)
	cat >"$config" <<'EOF'
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]
  runtime_type = 'io.containerd.gcr.v2'
  pod_annotations = ['gcr.dev/*']
EOF
	;;
esac

restore() {
	systemctl stop "containerd-$version" 2>/dev/null || true
	systemctl reset-failed "containerd-$version" 2>/dev/null || true
	systemctl start containerd
}
trap restore EXIT
systemctl stop containerd
systemd-run --unit "containerd-$version" --property Delegate=yes --property KillMode=process \
	--setenv PATH="$dir/bin:$PATH" "$dir/bin/containerd" --config "$config"
for _ in $(seq 50); do
	"$dir/bin/ctr" version >/dev/null 2>&1 && break
	sleep 0.2
done
"$dir/bin/ctr" version | grep -A1 Server
# CRI comes up after the daemon (1.x says "server is not initialized yet").
if command -v crictl >/dev/null; then
	for _ in $(seq 100); do
		crictl -r unix:///run/containerd/containerd.sock info >/dev/null 2>&1 && break
		sleep 0.2
	done
fi
"$@"
