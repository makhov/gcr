#!/bin/bash
# The installer DaemonSet (deploy/gcr.yaml) on a one-node k0da cluster
# (k0s in a container, without gcr): a release and its image are built for
# this host's architecture and loaded into the cluster, and the DaemonSet
# installs gcr, through k0s's containerd drop-ins. Checks the node is
# labelled ready, that a pod with
# runtimeClassName gcr runs on gcr's shim, and that deleting the DaemonSet
# unlabels the node.
#
# k0s's node image is Alpine, where gcr's CRIU (glibc) does not run: gcr
# doctor says so, and the rest works without it.
#
# Needs k0da, Docker (DOCKER="sudo docker" where its socket is root's), and
# what `make release` and `make image` need.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-gcr-install}
DOCKER=${DOCKER:-docker}
ARCH=$(go env GOARCH)
VERSION=v0.0.0-install
IMAGE=ghcr.io/makhov/gcr:$VERSION

node() { $DOCKER exec -i "$CLUSTER" "$@"; }
kc() { node k0s kubectl "$@"; }
fail() {
	echo "FAIL: $*"
	kc get pods -A -o wide || true
	kc -n gcr-system logs ds/gcr-installer --tail 40 || true
	exit 1
}
work=$(mktemp -d)
cleanup() { [ -n "${KEEP:-}" ] || k0da delete -n "$CLUSTER" >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT

echo "== release and image ($ARCH)"
make -s release VERSION=$VERSION RELEASE_ARCHES="$ARCH" DOCKER="$DOCKER" >/dev/null
make -s image VERSION=$VERSION RELEASE_ARCHES="$ARCH" DOCKER="$DOCKER" >/dev/null
$DOCKER save -o "$work/image.tar" "$IMAGE"

echo "== cluster"
k0da delete -n "$CLUSTER" >/dev/null 2>&1 || true
k0da create -n "$CLUSTER" -c hack/k8s/install-cluster.yaml --timeout 300s >/dev/null
k0da load archive "$work/image.tar" -n "$CLUSTER" >/dev/null

echo "== install"
kc apply -f - <deploy/gcr.yaml >/dev/null
kc -n gcr-system set image ds/gcr-installer install="$IMAGE" >/dev/null
kc wait node "$CLUSTER" --for=jsonpath='{.metadata.labels.gcr\.dev/runtime}'=ready --timeout 180s >/dev/null ||
	fail "the node was not labelled ready"
kc -n gcr-system logs ds/gcr-installer | sed 's/^/  /'

echo "== a pod on gcr"
kc apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: on-gcr}
spec:
  runtimeClassName: gcr
  containers:
    - name: c
      image: busybox:1.37
      command: [sh, -c, "i=0; while :; do i=\$((i+1)); echo \$i > /count; sleep 0.2; done"]
YAML
kc wait pod on-gcr --for=condition=Ready --timeout 120s >/dev/null || fail "the pod did not start"
[ "$(kc exec on-gcr -- cat /count)" -gt 0 ] || fail "exec"
id=$(kc get pod on-gcr -o jsonpath='{.status.containerStatuses[0].containerID}')
node k0s ctr -n k8s.io containers info "${id#containerd://}" | grep -q '"Name": "io.containerd.gcr.v2"' ||
	fail "the pod's container does not run on gcr"
echo "  runs on gcr"

echo "== uninstall"
kc delete pod on-gcr --wait >/dev/null
kc -n gcr-system delete ds gcr-installer --wait >/dev/null
for _ in $(seq 60); do
	[ -z "$(kc get node "$CLUSTER" -o jsonpath='{.metadata.labels.gcr\.dev/runtime}')" ] && break
	sleep 1
done
[ -z "$(kc get node "$CLUSTER" -o jsonpath='{.metadata.labels.gcr\.dev/runtime}')" ] ||
	fail "the node is still labelled after the installer went"
echo "  node unlabelled"
echo PASS
