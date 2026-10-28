#!/bin/bash
# Quick CRI smoke test of the gcr runtime handler with crictl: pod, container,
# exec (with and without a terminal), stats, resource update, logs.
set -euo pipefail
D=$(mktemp -d)
trap 'crictl rmp -f "$POD" >/dev/null 2>&1 || true; rm -rf "$D"' EXIT

cat > "$D/pod.json" <<EOF
{"metadata": {"name": "smoke", "namespace": "default", "uid": "smoke-$$"}, "log_directory": "$D",
 "annotations": {"gcr.dev/host-users": "false"}}
EOF
cat > "$D/ctr.json" <<EOF
{"metadata": {"name": "app"}, "image": {"image": "docker.io/library/busybox:latest"},
 "command": ["sh", "-c", "echo hello from gcr; sleep 3600"], "log_path": "app.log"}
EOF

crictl pull docker.io/library/busybox:latest >/dev/null
POD=$(crictl runp --runtime gcr "$D/pod.json")
CTR=$(crictl create "$POD" "$D/ctr.json" "$D/pod.json")
crictl start "$CTR" >/dev/null
sleep 1

echo "== exec: $(crictl exec "$CTR" sh -c 'echo $((6*7)) from pid $$')"
code=0; crictl exec "$CTR" sh -c 'exit 3' >/dev/null 2>&1 || code=$?
echo "== exec exit code: $code (crictl reports any failure as 1; ctr shows the real code)"
echo "== exec tty: $(script -qc "crictl exec -it $CTR tty" /dev/null </dev/null | tr -d '\r')"
echo "== logs: $(crictl logs "$CTR")"
crictl update --memory 67108864 "$CTR" >/dev/null
echo "== memory.max after update: $(crictl exec "$CTR" cat /sys/fs/cgroup/memory.max)"
echo "== stats: $(crictl stats -o json "$CTR" | jq -c '.stats[0] | {cpu: .cpu.usageCoreNanoSeconds.value, mem: .memory.workingSetBytes.value}')"

echo "== user namespace"
uidmap=$(crictl exec "$CTR" cat /proc/self/uid_map | tr -s ' ')
echo "uid_map:$uidmap"
case "$uidmap" in " 0 1073"*" 65536") ;; *) echo "FAIL: not in a gcr user namespace"; exit 1;; esac
owner=$(crictl exec "$CTR" stat -c %u:%g /bin/sh)
[ "$owner" = "0:0" ] || { echo "FAIL: image files owned by $owner inside"; exit 1; }
crictl exec "$CTR" sh -c 'echo ok > /written && chown 1000 /written' || { echo "FAIL: cannot write the rootfs as root"; exit 1; }
on_disk=$(stat -c %u "$(find /var/lib/containerd -path '*snapshots*' -name written 2>/dev/null | head -1)")
echo "file written as container root is uid $on_disk-ish on the host (1000 inside -> base+1000)"
crictl exec "$CTR" sh -c 'nc -l -p 80 -w 1 </dev/null & sleep 0.3; netstat -ltn | grep -q ":80 "' || { echo "FAIL: cannot bind port 80"; exit 1; }
echo PASS
