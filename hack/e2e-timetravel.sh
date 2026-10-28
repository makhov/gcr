#!/bin/bash
# End-to-end time travel: a counter that execs /bin/true every 0.2s (each exec
# can take a point) and deletes /keep at count 25. Rewinding to the first
# point must bring back an early count and /keep, keep the container running,
# and keep recording points afterwards.
set -euo pipefail

RUNTIME=io.containerd.gcr.v2
IMAGE=docker.io/library/busybox:latest
ID=gcr-tt-$$
ROOTFS=/run/containerd/io.containerd.runtime.v2.task/default/$ID/rootfs

cleanup() {
	ctr task kill -s KILL "$ID" >/dev/null 2>&1 || true
	sleep 0.5
	ctr task rm -f "$ID" >/dev/null 2>&1 || true
	ctr container rm "$ID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() {
	echo "FAIL: $*"
	gcr timeline "$ID" || true
	exit 1
}

ctr image pull "$IMAGE" >/dev/null

echo "== run with time travel"
ctr run -d --runtime "$RUNTIME" \
	--annotation gcr.dev/time-travel=execve \
	--annotation gcr.dev/time-travel-interval=1s \
	"$IMAGE" "$ID" sh -c '
		echo keep > /keep
		i=0
		while true; do
			i=$((i+1)); echo $i > /tmp/count
			[ $i -eq 25 ] && rm /keep
			/bin/true
			sleep 0.2
		done'
sleep 8
before=$(cat "$ROOTFS/tmp/count")
[ ! -e "$ROOTFS/keep" ] || fail "/keep should be gone at count $before"
echo "count=$before, /keep deleted"

echo "== timeline"
gcr timeline "$ID"
first=$(gcr timeline "$ID" | awk 'NR>1 {sub("#","",$1); print $1}' | sort -n | head -1)
last=$(gcr timeline "$ID" | awk 'NR>1 {sub("#","",$1); print $1}' | sort -n | tail -1)
[ -n "$first" ] || fail "no points recorded"

echo "== rewind to #$first"
gcr rewind "$ID" "$first"
rewound_at=$(date +%s.%N)
sleep 0.5
after=$(cat "$ROOTFS/tmp/count")
echo "count right after rewind: $after"
[ "$after" -lt "$before" ] || fail "count did not go back ($before -> $after)"
[ -e "$ROOTFS/keep" ] || fail "/keep was not restored"
ctr task ls | grep "$ID" | grep -q RUNNING || fail "task is not running"

sleep 4
later=$(cat "$ROOTFS/tmp/count")
# Same pace as before the rewind (~4/s): execs are not held or failing.
[ "$((later - after))" -ge 10 ] || fail "counter too slow after rewind ($after -> $later in 4s)"
# Points taken after the rewind prove the supervisor got the restored listener.
new_points=$(ctr task ls >/dev/null; gcr timeline "$ID" | awk -v t="$(date -d @"$rewound_at" +%H:%M:%S.%3N)" 'NR>1 && $2 > t' | wc -l)
[ "$new_points" -ge 2 ] || fail "only $new_points points recorded after rewind"
echo "count 4s later: $later; $new_points new points since the rewind"
gcr timeline "$ID"
echo PASS
