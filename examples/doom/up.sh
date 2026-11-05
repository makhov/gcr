#!/bin/bash
# A three-node k0da cluster (a controller and two workers) with gcr
# installed from its latest release's manifests, Traefik answering on
# http://localhost:8090, and Doom moving between the nodes
# (docs/examples/doom.md). Needs k0da v0.5.0 or later, and docker or
# podman.
set -euo pipefail

CLUSTER=${CLUSTER:-doom}
DIR=$(cd "$(dirname "$0")" && pwd)
RELEASE=https://github.com/makhov/gcr/releases/latest/download
export KUBECONFIG=${KUBECONFIG:-$HOME/.kube/$CLUSTER.kubeconfig}

k0da delete -n "$CLUSTER" >/dev/null 2>&1 || true
mkdir -p "$(dirname "$KUBECONFIG")"
rm -f "$KUBECONFIG" # k0da adds the cluster's context to it
k0da create -n "$CLUSTER" -c "$DIR/cluster.yaml" --timeout 600s | tail -1
for _ in $(seq 60); do
	[ "$(kubectl get nodes --no-headers 2>/dev/null | grep -c ' Ready')" -ge 3 ] && break
	sleep 5
done

echo "== gcr"
kubectl apply -f "$RELEASE/gcr.yaml" >/dev/null
kubectl apply -f "$RELEASE/gcr-operator.yaml" >/dev/null
for n in $(kubectl get nodes -o name); do
	kubectl wait "$n" --for=jsonpath='{.metadata.labels.gcr\.dev/runtime}'=ready --timeout 300s
done
kubectl -n gcr-system rollout status deploy/gcr-operator --timeout=300s

echo "== doom"
kubectl apply -f "$DIR/doom.yaml" >/dev/null
# k0s installs Traefik (cluster.yaml) once the cluster is up.
for _ in $(seq 60); do
	kubectl -n traefik get deploy/traefik >/dev/null 2>&1 && break
	sleep 5
done
kubectl -n traefik rollout status deploy/traefik --timeout=300s
kubectl rollout status deploy/doom --timeout=300s
kubectl rollout status deploy/arcade --timeout=300s
echo "Play at http://localhost:8090 (KUBECONFIG=$KUBECONFIG)"
