#!/bin/bash
# Removes what a failed end-to-end run left behind: containers in the
# namespaces the tests use, CRI pods, and the state directories. A leaked
# counter container writes a file ten times a second for as long as it
# lives, so one failed run makes the next one slower.
set -uo pipefail

for pod in $(crictl pods -q 2>/dev/null); do
	crictl rmp -f "$pod" >/dev/null 2>&1
done

for ns in $(ctr namespace ls -q 2>/dev/null); do
	case "$ns" in
	gcr-e2e | gcr-e2e-src | gcr-e2e-dst | default | src | dst) ;;
	*) continue ;;
	esac
	for id in $(ctr -n "$ns" container ls -q 2>/dev/null); do
		ctr -n "$ns" task kill -s KILL "$id" >/dev/null 2>&1
	done
	sleep 0.5
	for id in $(ctr -n "$ns" container ls -q 2>/dev/null); do
		ctr -n "$ns" task rm -f "$id" >/dev/null 2>&1
		ctr -n "$ns" container rm "$id" >/dev/null 2>&1
	done
done

pkill -f containerd-shim-gcr-v2 >/dev/null 2>&1
echo "left: $(pgrep -c -f containerd-shim-gcr-v2 || echo 0) shims, $(crictl pods -q 2>/dev/null | wc -l) pods"
