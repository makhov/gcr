#!/bin/bash
# Reproduce a failing migration without cleanup and print the source's
# CRIU dump log. Leaves containers "dbg" in namespaces src and dst.
set -u
ID=dbg
IMAGE=docker.io/library/busybox:latest
CMD=(sh -c 'i=0; while true; do i=$((i+1)); echo $i > /tmp/count; sleep 0.1; done')
for ns in src dst; do
	ctr -n $ns task kill -s KILL $ID >/dev/null 2>&1; sleep 0.3
	ctr -n $ns task rm -f $ID >/dev/null 2>&1; ctr -n $ns container rm $ID >/dev/null 2>&1
done
ctr -n src run -d --runtime io.containerd.gcr.v2 "$IMAGE" $ID "${CMD[@]}"
ctr -n dst run -d --runtime io.containerd.gcr.v2 --annotation gcr.dev/migrate-in=127.0.0.1:7782 \
	--annotation gcr.dev/migrate-token=t "$IMAGE" $ID "${CMD[@]}" >/dev/null 2>&1 &
sleep 2
gcr -n src migrate -token t $ID http://127.0.0.1:7782
log=$(ls /var/lib/gcr/src/$ID/migrate-out/*/dump.log | tail -1)
echo "== $log"
grep -vE "Handling VMA|smaps|type (ext4|vfat|overlay|proc|tmpfs|sysfs)" "$log" | head -${LINES_:-60}
