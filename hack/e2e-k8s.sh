#!/bin/bash
# Kubernetes on gcr: a one-node k0da (k0s in a container) cluster whose node
# image carries gcr. Checks what kubelet does through it: pods start, exec,
# logs, stats, and both kinds of user namespaces.
#
# Needs: docker or podman, k0da, and `make static` done. K0DA_RUNTIME picks
# the engine (default: whatever k0da detects).
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-gcr-e2e}
IMAGE=localhost/k0s-gcr:e2e
ENGINE=${K0DA_RUNTIME:-$(command -v docker >/dev/null && echo docker || echo podman)}

fail() { echo "FAIL: $*"; kc get pods -o wide || true; kc describe pods | tail -40 || true; exit 1; }
node() { "$ENGINE" exec -i "$CLUSTER" "$@"; }
kc() { node k0s kubectl "$@"; }

cleanup() { [ -n "${KEEP:-}" ] || k0da delete "$CLUSTER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== node image"
"$ENGINE" build -q -f hack/k8s/Containerfile -t "$IMAGE" . >/dev/null

echo "== cluster"
k0da delete "$CLUSTER" >/dev/null 2>&1 || true
# The node image goes in the cluster config: k0da create --image keeps only
# the k0s version from it.
k0da create "$CLUSTER" -c hack/k8s/cluster.yaml --timeout 300s
[ "$("$ENGINE" inspect "$CLUSTER" --format '{{.Config.Image}}')" = "$IMAGE" ] || fail "node is not running $IMAGE"
for _ in $(seq 60); do kc get nodes 2>/dev/null | grep -q " Ready" && break; sleep 5; done
kc get nodes

echo "== workloads"
"$ENGINE" cp hack/k8s/workloads.yaml "$CLUSTER":/tmp/workloads.yaml
kc apply -f /tmp/workloads.yaml
kc wait --for=condition=Ready pod --all --timeout=300s || fail "pods not ready"
kc get pods -o wide

echo "== exec, exit code, stdin"
[ "$(kc exec plain -- sh -c 'echo $((6*7))')" = 42 ] || fail "exec output"
code=0; kc exec plain -- sh -c 'exit 3' || code=$?
[ "$code" = 3 ] || fail "exec exit code $code, want 3"
[ "$(echo piped | kc exec -i plain -- cat)" = piped ] || fail "exec stdin"

echo "== logs"
kc logs plain | grep -q "hello from gcr" || fail "logs"

echo "== resource limits"
[ "$(kc exec plain -- cat /sys/fs/cgroup/memory.max)" = 67108864 ] || fail "memory limit"

echo "== stats (kubelet summary)"
for _ in $(seq 30); do
	cpu=$(kc get --raw "/api/v1/nodes/$CLUSTER/proxy/stats/summary" |
		jq -r '.pods[] | select(.podRef.name=="plain") | .containers[0].cpu.usageCoreNanoSeconds // empty')
	[ -n "$cpu" ] && break
	sleep 2
done
[ -n "$cpu" ] || fail "no container stats in the kubelet summary"
echo "plain: cpu ${cpu}ns"

echo "== user namespaces"
host=$(kc exec plain -- cat /proc/self/uid_map | tr -s ' ')
k8s=$(kc exec hostusers-false -- cat /proc/self/uid_map | tr -s ' ')
own=$(kc exec gcr-userns -c app -- cat /proc/self/uid_map | tr -s ' ')
side=$(kc exec gcr-userns -c sidecar -- cat /proc/self/uid_map | tr -s ' ')
echo "plain:$host | hostUsers=false:$k8s | gcr:$own | gcr sidecar:$side"
[ "$host" = " 0 0 4294967295" ] || fail "plain pod is not in the host user namespace"
case "$k8s" in " 0 0 "*) fail "hostUsers: false pod has no user namespace";; esac
case "$own" in " 0 1073"*" 65536") ;; *) fail "gcr.dev/host-users=false pod not in a gcr range";; esac
[ "$own" = "$side" ] || fail "pod containers have different ranges"
[ "$(kc exec gcr-userns -c app -- stat -c %u /bin/sh)" = 0 ] || fail "image files not root-owned inside"

echo PASS
