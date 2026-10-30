#!/bin/sh
# Installs gcr on the node this pod runs on (gcr.yaml's DaemonSet): copies
# this image's gcr to the host, runs `gcr setup` there, in the host's
# namespaces, then labels the node gcr.dev/runtime=ready, which the gcr
# RuntimeClass schedules pods by. The label goes when the pod stops.
#
# The host's / is mounted at /host, and its PID 1 is visible (hostPID).
# GCR_SETUP_ARGS are passed on to gcr setup; NODE_NAME names the node.
set -eu

mkdir -p /host/var/lib/gcr/install
cp /gcr /host/var/lib/gcr/install/gcr.new
mv /host/var/lib/gcr/install/gcr.new /host/var/lib/gcr/install/gcr

# shellcheck disable=SC2086 # the arguments are words
nsenter --target 1 --mount --uts --ipc --net --pid -- /var/lib/gcr/install/gcr setup ${GCR_SETUP_ARGS:-}

/gcr kube-label "$NODE_NAME" gcr.dev/runtime=ready
echo "gcr is ready on $NODE_NAME"

trap '/gcr kube-label "$NODE_NAME" gcr.dev/runtime-; exit 0' TERM INT
while :; do sleep 3600 & wait $!; done
