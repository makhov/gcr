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
mounts() { # extra ctr flags...
	ctr run --rm --runtime "$RUNTIME" "$@" "$IMAGE" "$ID" sh -c '
		try() { name=$1; shift; if "$@" 2>/dev/null; then echo "$name OK"; else echo "$name EPERM"; fi; }
		mkdir -p /mnt /opt /media
		try tmpfs mount -t tmpfs none /mnt
		grep -q " /mnt tmpfs " /proc/mounts && echo "visible OK" || echo "visible MISSING"
		try ramfs mount -t ramfs none /opt
		try proc mount -t proc proc /media
		mkdir -p /srv/a /srv/b; try bind mount --bind /srv/a /srv/b
		# A bind remount: busybox passes a plain remount the options from
		# /proc/mounts, where tmpfs shows its owner as an unmapped host ID.
		try remount mount -o remount,bind,ro /mnt
	' | sort
}
check() { # output line want
	echo "$1" | grep -qx "$2" || fail "want '$2' in: $(echo "$1" | tr '\n' ' ')"
}

echo "== supervised mount (CAP_SYS_ADMIN, user namespace)"
out=$(mounts --cap-add CAP_SYS_ADMIN)
echo "$out" | column -t
for l in "tmpfs OK" "visible OK" "ramfs EPERM" "proc OK" "bind OK" "remount OK"; do check "$out" "$l"; done

echo "== supervised mount (CAP_SYS_ADMIN, host users)"
out=$(mounts --cap-add CAP_SYS_ADMIN --annotation gcr.dev/host-users=true)
for l in "tmpfs OK" "ramfs EPERM" "proc OK"; do check "$out" "$l"; done
echo "tmpfs and proc allowed, ramfs refused"

echo "== no CAP_SYS_ADMIN: the kernel refuses, gcr does not mount for it"
out=$(mounts)
check "$out" "tmpfs EPERM"
echo "tmpfs refused"
echo PASS
