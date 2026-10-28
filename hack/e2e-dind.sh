#!/bin/bash
# Docker-in-Docker in a gcr pod with a user namespace, without privileged:
# dockerd with CAP_SYS_ADMIN and CAP_NET_ADMIN (scoped to the pod's user
# and network namespaces), RuntimeDefault seccomp plus gcr's hardening and
# supervised mounts, and a delegated cgroup. Runs and builds nested
# containers.
#
# The pod gets its user namespace from CRI, as with Kubernetes hostUsers:
# false: containerd then creates the pod's network namespace inside it,
# which dockerd needs to manage (a network namespace the host owns cannot
# be configured from a user namespace).
set -euo pipefail
D=$(mktemp -d)
POD=
trap 'if [ -n "$POD" ]; then crictl rmp -f "$POD" >/dev/null 2>&1 || true; fi; rm -rf "$D"' EXIT
fail() { echo "FAIL: $*"; [ -n "$POD" ] && tail -20 "$D/logs/dockerd.log"; exit 1; }

# Host IDs for the pod (what kubelet would pick for hostUsers: false).
BASE=1879048192
USERNS="\"userns_options\": {\"mode\": 0,
  \"uids\": [{\"host_id\": $BASE, \"container_id\": 0, \"length\": 65536}],
  \"gids\": [{\"host_id\": $BASE, \"container_id\": 0, \"length\": 65536}]}"

# dockerd's state; kubelet would make a volume the pod's root can use.
mkdir -p "$D/logs" "$D/docker"
chown "$BASE:$BASE" "$D/docker"

cat >"$D/pod.json" <<EOF
{"metadata": {"name": "dind", "namespace": "default", "uid": "dind-$$"}, "log_directory": "$D/logs",
 "dns_config": {"servers": ["1.1.1.1", "8.8.8.8"]},
 "linux": {"security_context": {"namespace_options": {$USERNS}}}}
EOF
# --iptables=false and --bridge=none: nested containers share the pod's
# network (--network host). AppArmor unconfined, as for privileged DinD:
# the nested runtime's processes otherwise end up under labels the
# default profile does not let signal each other.
cat >"$D/dockerd.json" <<EOF
{"metadata": {"name": "dockerd"}, "image": {"image": "docker.io/library/docker:dind"},
 "command": ["dockerd-entrypoint.sh"],
 "args": ["dockerd", "--iptables=false", "--ip6tables=false", "--bridge=none", "--host=unix:///var/run/docker.sock"],
 "envs": [{"key": "DOCKER_TLS_CERTDIR", "value": ""}],
 "mounts": [{"container_path": "/var/lib/docker", "host_path": "$D/docker"}],
 "linux": {"security_context": {
   "capabilities": {"add_capabilities": ["SYS_ADMIN", "NET_ADMIN"]},
   "seccomp": {"profile_type": 0},
   "apparmor": {"profile_type": 1},
   "namespace_options": {"network": 0, "pid": 1, $USERNS}}},
 "log_path": "dockerd.log"}
EOF

crictl pull docker.io/library/docker:dind >/dev/null
POD=$(crictl runp --runtime gcr "$D/pod.json")
CTR=$(crictl create "$POD" "$D/dockerd.json" "$D/pod.json")
crictl start "$CTR" >/dev/null
for _ in $(seq 60); do
	crictl exec "$CTR" docker info >/dev/null 2>&1 && break
	sleep 1
done
crictl exec "$CTR" docker info >/dev/null 2>&1 || fail "dockerd did not start"

in_pod() { timeout -s KILL 300 crictl exec "$CTR" sh -c "$1" 2>&1; }

echo "== dockerd"
in_pod 'docker info --format "{{.Driver}} {{.CgroupDriver}}/{{.CgroupVersion}} {{.SecurityOptions}}"'
uidmap=$(in_pod 'cat /proc/self/uid_map' | tr -s ' ')
echo "uid_map:$uidmap"
[ "$uidmap" = " 0 $BASE 65536" ] || fail "dockerd not in the pod's user namespace"

echo "== nested docker run"
out=$(in_pod 'docker run --rm --network host busybox echo hello from nested docker') || fail "docker run: $out"
echo "$out" | tail -1
echo "$out" | grep -qx "hello from nested docker" || fail "docker run: $out"
nested=$(in_pod 'docker run --rm --network host busybox cat /proc/self/uid_map' | tail -1 | tr -s ' ')
[ "$nested" = "$uidmap" ] || fail "nested container's uid_map: $nested"

echo "== nested docker build"
out=$(in_pod 'printf "FROM busybox\nRUN echo built inside > /built\nCMD cat /built\n" |
	docker build -q --network host -t nested - >/dev/null && docker run --rm --network host nested') ||
	fail "docker build: $out"
echo "$out" | tail -1
echo "$out" | grep -qx "built inside" || fail "docker build: $out"

echo "== gcr's filter holds for nested containers without one of their own"
out=$(in_pod 'docker run --rm --network host --cap-add SYS_ADMIN --security-opt seccomp=unconfined \
	--security-opt apparmor=unconfined busybox sh -c "mkdir -p /mnt
	mount -t ramfs r /mnt 2>/dev/null && echo ramfs mounted || echo ramfs refused
	mount -t tmpfs t /mnt && echo tmpfs mounted"') || fail "nested mounts: $out"
echo "$out" | tail -2
echo "$out" | grep -qx "ramfs refused" || fail "nested container mounted ramfs"
echo "$out" | grep -qx "tmpfs mounted" || fail "nested container could not mount tmpfs"
echo PASS
