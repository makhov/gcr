#!/bin/bash
# Landlock: a host directory descriptor that reaches a container (the way
# runc's CVE-2024-21626 leaked one) leads nowhere from the container's
# processes, also after a checkpoint and restore; with Landlock = off it
# does. Processes started with exec stay outside, so they can still
# inspect the container's processes.
set -euo pipefail
cd "$(dirname "$0")/.."

RUNTIME=io.containerd.gcr.v2
IMAGE=docker.io/library/busybox:latest
CKPT=localhost/gcr-landlock-ckpt:latest
ID=gcr-landlock-$$
D=$(mktemp -d)
SERVER=
cleanup() {
	ctr task kill -s KILL "$ID" >/dev/null 2>&1 || true
	ctr task rm -f "$ID" >/dev/null 2>&1 || true
	ctr container rm "$ID" >/dev/null 2>&1 || true
	ctr image rm "$CKPT" >/dev/null 2>&1 || true
	if [ -n "$SERVER" ]; then kill "$SERVER" 2>/dev/null || true; fi
	rm -rf "$D"
}
trap cleanup EXIT
fail() { echo "FAIL: $*"; exit 1; }

cc -static -O2 -o "$D/probe" hack/hardening/probe.c
ctr image pull "$IMAGE" >/dev/null

# A host directory with a secret the container's root could read, and a
# socket that hands out a descriptor for it.
mkdir -p "$D/host" "$D/sock"
echo host-secret >"$D/host/secret"
chmod 0755 "$D" "$D/host" && chmod 0644 "$D/host/secret" && chmod 0777 "$D/sock"
python3 - "$D/sock/fd.sock" "$D/host" <<'EOF' &
import os, socket, sys
s = socket.socket(socket.AF_UNIX)
s.bind(sys.argv[1])
os.chmod(sys.argv[1], 0o777)
s.listen()
fd = os.open(sys.argv[2], os.O_PATH | os.O_DIRECTORY)
while True:
    c, _ = s.accept()
    socket.send_fds(c, [b"x"], [fd])
    c.close()
EOF
SERVER=$!
for _ in $(seq 50); do [ -S "$D/sock/fd.sock" ] && break; sleep 0.1; done

RESULT=/run/containerd/io.containerd.runtime.v2.task/default/$ID/rootfs/tmp/result
run() { # extra ctr flags...
	ctr run -d --runtime "$RUNTIME" "$@" \
		--mount "type=bind,src=$D/probe,dst=/probe,options=rbind:ro" \
		--mount "type=bind,src=$D/sock,dst=/sock,options=rbind" \
		"$IMAGE" "$ID" sh -c 'while true; do /probe leak /sock/fd.sock >/tmp/result.new; mv /tmp/result.new /tmp/result; sleep 0.5; done'
}
result() { # waits for a fresh result
	rm -f "$RESULT"
	for _ in $(seq 40); do
		[ -s "$RESULT" ] && { cat "$RESULT"; return; }
		sleep 0.25
	done
	echo "no result"
}
stop() {
	ctr task kill -s KILL "$ID" >/dev/null 2>&1 || true
	sleep 0.5
	ctr task rm -f "$ID" >/dev/null 2>&1 || true
	ctr container rm "$ID" >/dev/null
}

echo "== Landlock = off: the leaked descriptor reaches the host"
printf 'Landlock = "off"\n' >"$D/off.toml"
run --runtime-config-path "$D/off.toml"
got=$(result)
echo "$got"
[ "$got" = "leak OK host-secret" ] || fail "with Landlock off: $got"
stop

echo "== Landlock = standard: it does not"
run
got=$(result)
echo "$got"
[ "$got" = "leak EACCES" ] || fail "confined container: $got"

echo "== exec stays outside, so it can inspect the container's processes"
ctr task exec --exec-id env "$ID" sh -c 'tr "\0" "\n" </proc/1/environ | grep -q PATH' ||
	fail "exec cannot read /proc/1/environ"
echo "exec read /proc/1/environ"

echo "== after checkpoint and restore"
ctr container checkpoint --task --rw "$ID" "$CKPT"
ctr task kill -s KILL "$ID"
sleep 1
ctr task rm "$ID"
ctr container rm "$ID"
ctr container restore --live --rw "$ID" "$CKPT" &
sleep 2
ctr task ls | grep -q "$ID" || fail "not restored"
got=$(result)
echo "$got"
[ "$got" = "leak EACCES" ] || fail "restored container: $got"
echo PASS
