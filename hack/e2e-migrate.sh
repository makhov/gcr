#!/bin/bash
# End-to-end migration on one host, from containerd namespace "src" to "dst".
# A counter migrates and must carry on from where it was; a migration the
# target refuses must leave the source running.
set -euo pipefail

RUNTIME=io.containerd.gcr.v2
IMAGE=docker.io/library/busybox:latest
ID=gcr-mig-$$
ADDR=127.0.0.1:7781
TOKEN=secret-$$
CMD=(sh -c 'i=0; while true; do i=$((i+1)); echo $i > /tmp/count; sleep 0.1; done')
rootfs() { echo "/run/containerd/io.containerd.runtime.v2.task/$1/$ID/rootfs"; }

cleanup() {
	for ns in src dst; do
		ctr -n $ns task kill -s KILL "$ID" >/dev/null 2>&1 || true
	done
	sleep 0.5
	for ns in src dst; do
		ctr -n $ns task rm -f "$ID" >/dev/null 2>&1 || true
		ctr -n $ns container rm "$ID" >/dev/null 2>&1 || true
	done
	jobs -p | xargs -r kill 2>/dev/null || true
}
trap cleanup EXIT
fail() { echo "FAIL: $*"; exit 1; }

for ns in src dst; do ctr -n $ns image pull "$IMAGE" >/dev/null; done

echo "== source"
ctr -n src run -d --runtime "$RUNTIME" "$IMAGE" "$ID" "${CMD[@]}"
sleep 2

echo "== a refused migration leaves the source running"
ctr -n dst run -d --runtime "$RUNTIME" \
	--annotation gcr.dev/migrate-in=$ADDR --annotation gcr.dev/migrate-token=$TOKEN \
	"$IMAGE" "$ID" "${CMD[@]}" >/dev/null 2>&1 &
sleep 1
if gcr -n src migrate -token wrong "$ID" "http://$ADDR" 2>/dev/null; then
	fail "migration with a wrong token succeeded"
fi
a=$(cat "$(rootfs src)/tmp/count"); sleep 0.5; b=$(cat "$(rootfs src)/tmp/count")
[ "$b" -gt "$a" ] || fail "source stopped after a refused migration"
echo "source still counting ($a -> $b)"

echo "== migrate"
gcr -n src migrate -token "$TOKEN" "$ID" "http://$ADDR"
last=$(cat "$(rootfs src)/tmp/count")
sleep 1
ctr -n dst task ls | grep "$ID" | grep -q RUNNING || fail "target task is not running"
ctr -n src task ls | grep "$ID" | grep -q STOPPED || fail "source task is not stopped"
moved=$(cat "$(rootfs dst)/tmp/count")
echo "source stopped at $last; target at $moved one second later"
# A fresh start would be at ~10; carrying on means past the source's count.
[ "$moved" -gt "$last" ] || fail "target restarted instead of resuming ($last -> $moved)"
echo PASS
