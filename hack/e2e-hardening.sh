#!/bin/bash
# Runs hack/hardening/probe in containers with Hardening = standard (the
# default) and off, and checks what gets refused.
set -euo pipefail
cd "$(dirname "$0")/.."

RUNTIME=io.containerd.gcr.v2
IMAGE=docker.io/library/busybox:latest
ID=gcr-hard-$$
D=$(mktemp -d)
trap 'ctr container rm "$ID" >/dev/null 2>&1 || true; rm -rf "$D"' EXIT
fail() { echo "FAIL: $*"; exit 1; }

cc -static -O2 -o "$D/probe" hack/hardening/probe.c
ctr image pull "$IMAGE" >/dev/null

probe() {
	ctr run --rm --runtime "$RUNTIME" "$@" \
		--mount "type=bind,src=$D/probe,dst=/probe,options=rbind:ro" \
		"$IMAGE" "$ID" /probe
}

echo "== Hardening = standard"
out=$(probe)
echo "$out" | column -t
expect() { # name result
	got=$(echo "$out" | awk -v n="$1" '$1 == n {print $2}')
	[ "$got" = "$2" ] || fail "$1: got $got, want $2"
}
for c in io_uring_setup bpf perf_event_open userfaultfd keyctl fsopen open_tree init_module \
	open_by_handle_at unshare_user socket_packet socket_raw socket_vsock socket_alg; do
	expect "$c" EPERM
done
expect clone3 ENOSYS
for c in control_socket_tcp control_socket_ping control_fork; do
	expect "$c" OK
done

echo "== Hardening = off"
printf "Hardening = 'off'\n" >"$D/off.toml"
out=$(probe --runtime-config-path "$D/off.toml")
for c in io_uring_setup socket_alg clone3; do
	got=$(echo "$out" | awk -v n="$c" '$1 == n {print $2}')
	case "$got" in EPERM | ENOSYS) fail "$c still refused with Hardening = off ($got)" ;; esac
done
echo "io_uring_setup, AF_ALG and clone3 no longer refused"
echo PASS
