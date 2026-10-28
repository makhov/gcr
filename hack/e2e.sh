#!/bin/bash
# End-to-end: run a counter under gcr, checkpoint it, restore it, and check
# the counter continues from where it stopped instead of starting over.
set -euo pipefail

RUNTIME=io.containerd.gcr.v2
IMAGE=docker.io/library/busybox:latest
CKPT=localhost/gcr-e2e-ckpt:latest
ID=gcr-e2e-$$
COUNT=/run/containerd/io.containerd.runtime.v2.task/default/$ID/rootfs/tmp/count

cleanup() {
	ctr task kill -s KILL "$ID" >/dev/null 2>&1 || true
	ctr task rm -f "$ID" >/dev/null 2>&1 || true
	ctr container rm "$ID" >/dev/null 2>&1 || true
	ctr image rm "$CKPT" >/dev/null 2>&1 || true
}
trap cleanup EXIT

ctr image pull "$IMAGE" >/dev/null

echo "== run"
ctr run -d --runtime "$RUNTIME" "$IMAGE" "$ID" \
	sh -c 'i=0; while true; do echo $i > /tmp/count; i=$((i+1)); sleep 0.2; done'
sleep 4
ctr task ls | grep "$ID"
before=$(cat "$COUNT")
echo "counter before checkpoint: $before"
map_before=$(ctr task exec --exec-id m1 "$ID" cat /proc/self/uid_map | tr -s ' ')

echo "== checkpoint"
ctr container checkpoint --task --rw "$ID" "$CKPT"
ctr task kill -s KILL "$ID"
sleep 1
ctr task rm "$ID"
ctr container rm "$ID"

echo "== restore"
# --live restores the task's process state; ctr then waits on the task,
# so run it in the background.
ctr container restore --live --rw "$ID" "$CKPT" &
sleep 2
ctr task ls | grep "$ID"
after=$(cat "$COUNT")
echo "counter 2s after restore: $after"

# Same user namespace range after restore, and image files still root's.
map_after=$(ctr task exec --exec-id m2 "$ID" cat /proc/self/uid_map | tr -s ' ')
owner=$(ctr task exec --exec-id m3 "$ID" stat -c %u /bin/sh)
echo "uid_map before:$map_before after:$map_after; /bin/sh owner inside: $owner"
[ "$map_before" = "$map_after" ] || { echo "FAIL: user namespace mapping changed"; exit 1; }
[ "$owner" = 0 ] || { echo "FAIL: image files not root-owned inside after restore"; exit 1; }

# A fresh start would be at ~10 now; a restore continues past $before.
if [ "$after" -gt "$before" ]; then
	echo "PASS"
else
	echo "FAIL: counter restarted ($before -> $after)"
	exit 1
fi
