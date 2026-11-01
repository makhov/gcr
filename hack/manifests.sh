#!/bin/sh
# Writes deploy/gcr.yaml and deploy/gcr-operator.yaml, for kubectl apply,
# from the chart (charts/gcr), which is where to change them. CI checks
# they are up to date. hack/release.sh sets the release's image in them.
set -eu
cd "$(dirname "$0")/.."
HELM=${HELM:-helm}

# render FILE HEADER [HELM ARGS...]: the chart rendered into FILE, under
# HEADER as a comment.
render() {
	file=$1 header=$2
	shift 2
	{
		printf '%s\n\nGenerated from charts/gcr by `make manifests`: change the chart.\n' "$header" | sed 's/^/# /; s/^# $/#/'
		# Blank lines vary between helm versions; these manifests need none.
		$HELM template gcr charts/gcr --namespace gcr-system "$@" | grep -v -e '^# Source: ' -e '^$'
	} >"$file"
}

render deploy/gcr.yaml "gcr on every node of a Kubernetes cluster that runs containerd:

  kubectl apply -f gcr.yaml

A DaemonSet runs \`gcr setup\` on each node: it installs the shim and gcr's
CRIU, registers the runtime with containerd, and labels the node
gcr.dev/runtime=ready. Pods with runtimeClassName: gcr run there. The
chart (helm install gcr charts/gcr) does the same, with options." \
	--set createNamespace=true

render deploy/gcr-operator.yaml "gcr-operator: evictions of pods that opt in (gcr.dev/migrate-on-evict:
\"true\") become migrations to another node (docs/operator.md). Needs
gcr.yaml applied first.

  kubectl apply -f gcr-operator.yaml" \
	--set runtime.enabled=false --set operator.enabled=true
