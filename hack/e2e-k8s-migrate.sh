#!/bin/bash
# gcr-operator moving pods between nodes, as a cluster has them: a release
# and its image are built for this host's architecture and loaded into a
# three-node k0da cluster (hack/k8s/migrate-cluster.yaml) of stock k0s
# nodes, where the installer DaemonSet puts gcr, CRIU included, and
# gcr-operator runs (deploy/gcr.yaml, deploy/gcr-operator.yaml). Checks
# Migrations, pre-copy and post-copy, kubectl drain, a pod with several
# containers, and refusals. Each moved pod counts in memory: one that
# carries on where it was, rather than restarting, moved.
#
# Needs k0da, Docker (DOCKER="sudo docker" where its socket is root's), and
# what `make release` and `make image` need; a kernel gcr supports. REUSE=1
# skips the build when an earlier run's image is there.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-gcr-migrate}
DOCKER=${DOCKER:-docker}
ARCH=$(go env GOARCH)
VERSION=v0.0.0-migrate
IMAGE=ghcr.io/makhov/gcr:$VERSION

node() { $DOCKER exec -i "$CLUSTER" "$@"; }
kc() { node k0s kubectl "$@"; }
fail() {
	echo "FAIL: $*"
	kc get pods -A -o wide || true
	kc get migrations -o wide || true
	kc -n gcr-system logs deploy/gcr-operator --tail 60 || true
	exit 1
}
work=$(mktemp -d)
cleanup() { [ -n "${KEEP:-}" ] || k0da delete -n "$CLUSTER" >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT

echo "== release and image ($ARCH)"
# REUSE=1 takes the image an earlier run built, if it's there.
if [ -z "${REUSE:-}" ] || ! $DOCKER image inspect "$IMAGE" >/dev/null 2>&1; then
	make -s release VERSION=$VERSION RELEASE_ARCHES="$ARCH" DOCKER="$DOCKER" >/dev/null
	make -s image VERSION=$VERSION RELEASE_ARCHES="$ARCH" DOCKER="$DOCKER" >/dev/null
fi
$DOCKER save -o "$work/image.tar" "$IMAGE"

echo "== cluster"
k0da delete -n "$CLUSTER" >/dev/null 2>&1 || true
k0da create -n "$CLUSTER" -c hack/k8s/migrate-cluster.yaml --timeout 300s >/dev/null
k0da load archive "$work/image.tar" -n "$CLUSTER" >/dev/null
for _ in $(seq 60); do [ "$(kc get nodes --no-headers 2>/dev/null | grep -c ' Ready')" -ge 3 ] && break; sleep 5; done
kc get nodes

echo "== gcr and gcr-operator"
kc apply -f - <deploy/gcr.yaml >/dev/null
kc -n gcr-system set image ds/gcr-installer install="$IMAGE" >/dev/null
kc apply -f - <deploy/gcr-operator.yaml >/dev/null
kc -n gcr-system set image deploy/gcr-operator operator="$IMAGE" >/dev/null
for n in $(kc get nodes -o name); do
	kc wait "$n" --for=jsonpath='{.metadata.labels.gcr\.dev/runtime}'=ready --timeout 300s >/dev/null ||
		fail "gcr not ready on ${n#node/}"
done
kc -n gcr-system rollout status deploy/gcr-operator --timeout 300s >/dev/null || fail "gcr-operator did not start"

# A counter, in memory: /tmp/started is when it started, /tmp/n how far it
# got. One that moved carries on; one started anew starts again.
# shellcheck disable=SC2016 # expanded in the pod
counter='date +%s%N > /tmp/started; i=0; while :; do i=$((i+1)); echo $i > /tmp/n; sleep 0.1; done'
kc apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: counter}
spec:
  replicas: 1
  selector: {matchLabels: {app: counter}}
  template:
    metadata:
      labels: {app: counter}
      annotations: {gcr.dev/migrate-on-evict: "true"}
    spec:
      runtimeClassName: gcr
      terminationGracePeriodSeconds: 2
      containers: [{name: c, image: busybox:1.37, command: [sh, -c, '$counter']}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: pair}
spec:
  replicas: 1
  selector: {matchLabels: {app: pair}}
  template:
    metadata:
      labels: {app: pair}
      annotations: {gcr.dev/migrate-container: app}
    spec:
      runtimeClassName: gcr
      terminationGracePeriodSeconds: 2
      containers:
        - {name: app, image: busybox:1.37, command: [sh, -c, '$counter']}
        - {name: side, image: busybox:1.37, command: [sh, -c, '$counter']}
---
apiVersion: v1
kind: Pod
metadata: {name: unnamed}
spec:
  runtimeClassName: gcr
  terminationGracePeriodSeconds: 1
  containers:
    - {name: a, image: busybox:1.37, command: [sleep, "100000"]}
    - {name: b, image: busybox:1.37, command: [sleep, "100000"]}
---
apiVersion: v1
kind: Pod
metadata: {name: hostpath}
spec:
  runtimeClassName: gcr
  terminationGracePeriodSeconds: 1
  containers: [{name: c, image: busybox:1.37, command: [sleep, "100000"], volumeMounts: [{name: h, mountPath: /h}]}]
  volumes: [{name: h, hostPath: {path: /tmp}}]
YAML
kc rollout status deploy/counter --timeout 300s >/dev/null || fail "counter did not start"
kc rollout status deploy/pair --timeout 300s >/dev/null || fail "pair did not start"
kc wait pod unnamed hostpath --for=condition=Ready --timeout 300s >/dev/null || fail "pods did not start"
# A new node's kubelet may not have its serving certificate yet.
for _ in $(seq 30); do kc exec unnamed -c a -- true 2>/dev/null && break; sleep 2; done

pod() { kc get pod -l "app=$1" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}'; }
# state POD [CONTAINER]: "STARTED COUNT".
# shellcheck disable=SC2016 # expanded in the pod
state() { kc exec "$1" -c "${2:-c}" -- sh -c 'echo $(cat /tmp/started) $(cat /tmp/n)'; }
# migrate POD [SPEC...]: runs a Migration of POD, and prints its status.
migrate() {
	local name
	name=$(kc create -o name -f - <<YAML
apiVersion: gcr.dev/v1alpha1
kind: Migration
metadata: {generateName: e2e-}
spec: {pod: $1${2:+, $2}}
YAML
	)
	for _ in $(seq 300); do
		case "$(kc get "$name" -o jsonpath='{.status.phase}')" in Succeeded | Failed) break ;; esac
		sleep 1
	done
	kc get "$name" -o jsonpath='{.status.phase}|{.status.target}|{.status.node}|{.status.targetNode}|{.status.downtime}|{.status.memoryMoved}|{.status.message}'
}
# moved APP BEFORE [CONTAINER]: APP's pod carries on from BEFORE, on
# another node, its ReplicaSet's, named as its hostname says.
moved() {
	local p after
	p=$(pod "$1")
	after=$(state "$p" "${3:-c}")
	[ "${after% *}" = "${2% *}" ] || fail "$1 started anew ($2 -> $after)"
	[ "${after#* }" -gt "${2#* }" ] || fail "$1 did not carry on ($2 -> $after)"
	[ "$(kc get pod "$p" -o jsonpath='{.metadata.ownerReferences[0].kind}')" = ReplicaSet ] ||
		fail "$p was not handed over to its ReplicaSet"
	[ "$(kc exec "$p" -c "${3:-c}" -- hostname)" = "$p" ] || fail "$p's hostname is not its own"
	echo "  $p carried on: ${2#* } -> ${after#* }"
}

for mode in PreCopy PostCopy; do
	echo "== Migration, $mode"
	src=$(pod counter)
	before=$(state "$src")
	IFS='|' read -r phase target from to downtime memory message <<<"$(migrate "$src" "mode: $mode")"
	echo "  $phase: $from -> $to, downtime $downtime${memory:+, memory moved by $memory} $message"
	[ "$phase" = Succeeded ] || fail "the $mode Migration failed: $message"
	[ "$from" != "$to" ] && [ -n "$downtime" ] || fail "status: $from -> $to, downtime '$downtime'"
	[ "$mode" = PreCopy ] || [ -n "$memory" ] || fail "a post-copy Migration without memoryMoved"
	[ "$(pod counter)" = "$target" ] || fail "the pod is $(pod counter), not the target $target"
	kc get pod "$src" >/dev/null 2>&1 && fail "the source $src is still there"
	moved counter "$before"
done

echo "== kubectl drain"
src=$(pod counter)
from=$(kc get pod "$src" -o jsonpath='{.spec.nodeName}')
before=$(state "$src")
kc drain "$from" --ignore-daemonsets --delete-emptydir-data --timeout 300s >/dev/null 2>&1 || fail "drain of $from"
kc uncordon "$from" >/dev/null
[ "$(kc get pod "$(pod counter)" -o jsonpath='{.spec.nodeName}')" != "$from" ] || fail "counter is still on $from"
moved counter "$before"

echo "== a pod with two containers, one named"
src=$(pod pair)
before=$(state "$src" app)
side=$(state "$src" side)
IFS='|' read -r phase _ from to downtime _ message <<<"$(migrate "$src")"
echo "  $phase: $from -> $to, downtime $downtime $message"
[ "$phase" = Succeeded ] || fail "the Migration of pair failed: $message"
moved pair "$before" app
after=$(state "$(pod pair)" side)
[ "${after% *}" != "${side% *}" ] || fail "side moved too, rather than starting anew"
echo "  side started anew"

echo "== refusals"
for refusal in "unnamed:name the one" "hostpath:volume h"; do
	p=${refusal%%:*}
	IFS='|' read -r phase _ _ _ _ _ message <<<"$(migrate "$p")"
	echo "  $p: $phase: ${message:0:140}"
	[ "$phase" = Failed ] || fail "$p was not refused"
	case "$message" in *"${refusal#*:}"*) ;; *) fail "$p was refused for another reason" ;; esac
	kc wait pod "$p" --for=condition=Ready --timeout 10s >/dev/null || fail "$p is not running after the refusal"
done

echo PASS
