#!/bin/bash
# A three-node cluster (a controller and two workers) with gcr installed
# from its latest release's manifests, and Doom moving between the nodes,
# played at http://localhost:8090 (docs/examples/doom.md). Needs Linux
# 6.12 or later (on a Mac, in a Linux VM), docker or podman, and k0da
# v0.5.0 or later, or kind with TOOL=kind.
set -euo pipefail

CLUSTER=${CLUSTER:-doom}
TOOL=${TOOL:-k0da}
DIR=$(cd "$(dirname "$0")" && pwd)
RELEASE=https://github.com/makhov/gcr/releases/latest/download
export KUBECONFIG=${KUBECONFIG:-$HOME/.kube/$CLUSTER.kubeconfig}

mkdir -p "$(dirname "$KUBECONFIG")"
rm -f "$KUBECONFIG" # the tool adds the cluster's context to it
case "$TOOL" in
k0da)
	k0da delete -n "$CLUSTER" >/dev/null 2>&1 || true
	k0da create -n "$CLUSTER" -c "$DIR/cluster.yaml" --timeout 600s | tail -1
	;;
kind)
	# kind runs its nodes on docker, or on podman where docker isn't.
	if ! docker info >/dev/null 2>&1 && command -v podman >/dev/null; then
		export KIND_EXPERIMENTAL_PROVIDER=podman
	fi
	kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	if ! kind create cluster --name "$CLUSTER" --config "$DIR/kind.yaml" --kubeconfig "$KUBECONFIG"; then
		echo "If a worker's kubelet didn't start, the host may be out of inotify instances," >&2
		echo "as with more than one cluster: sysctl fs.inotify.max_user_instances=512." >&2
		exit 1
	fi
	;;
*)
	echo "TOOL is k0da or kind, not $TOOL" >&2
	exit 1
	;;
esac
for _ in $(seq 60); do
	[ "$(kubectl get nodes --no-headers 2>/dev/null | grep -c ' Ready')" -ge 3 ] && break
	sleep 5
done

# The nodes are containers: their kernel is the host's (or the VM's).
kernel=$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.kernelVersion}')
if [ "$(printf '%s\n' 6.12 "${kernel%%-*}" | sort -V | head -1)" != 6.12 ]; then
	echo "Linux $kernel is too old: gcr needs 6.12 or later." >&2
	echo "On a Mac, run this in a VM from a current image, such as" >&2
	echo "limactl start template:ubuntu-26.04 or template:debian-13." >&2
	exit 1
fi

echo "== gcr"
kubectl apply -f "$RELEASE/gcr.yaml" >/dev/null
kubectl apply -f "$RELEASE/gcr-operator.yaml" >/dev/null
for n in $(kubectl get nodes -o name); do
	if ! kubectl wait "$n" --for=jsonpath='{.metadata.labels.gcr\.dev/runtime}'=ready --timeout 300s; then
		echo "gcr's installer did not get ${n#node/} ready; its log:" >&2
		kubectl -n gcr-system logs ds/gcr-installer --tail=30 >&2 || true
		exit 1
	fi
done
kubectl -n gcr-system rollout status deploy/gcr-operator --timeout=300s
# The game moves post-copy, which needs CRIU to restore lazily: not in a
# node in a rootless container. The arcade then moves it pre-copy. Only
# CRIU's own answer counts: the exec can fail just after the install.
lazy=
for _ in $(seq 5); do
	out=$(kubectl -n gcr-system exec ds/gcr-installer -- nsenter -t 1 -m -- \
		/var/lib/gcr/bin/criu check --feature uffd-noncoop 2>&1) && { lazy=yes; break; }
	case "$out" in *"not supported"* | *"not permitted"*) lazy=no; break ;; esac
	sleep 2
done
if [ "$lazy" = no ]; then
	echo "Note: CRIU can't restore lazily on these nodes (is docker or podman rootless?)." >&2
	echo "Doom will move pre-copy, with a longer pause; create the cluster as root for post-copy." >&2
fi

echo "== doom"
kubectl apply -f "$DIR/doom.yaml" >/dev/null
kubectl rollout status deploy/doom --timeout=300s
kubectl rollout status deploy/arcade --timeout=300s
echo "Play at http://localhost:8090 (KUBECONFIG=$KUBECONFIG)"
