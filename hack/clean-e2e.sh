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

# Only this host's: the shims of clusters whose nodes are containers here
# (k0da, as on the dev VM, which also runs the self-hosted CI jobs) show
# up too, in mount namespaces of their own, and killing them takes their
# pods' containers down with them.
pkill --ns $$ --nslist mnt -f "[c]ontainerd-shim-gcr-v2" >/dev/null 2>&1
# Snapshots pulled lazily in a failed run stay mounted, each by a nydusd
# that exits with its mount.
findmnt -t fuse -n -o TARGET,SOURCE | awk '$2 == "rafs" && $1 ~ "^/tmp/Test" {print $1}' | xargs -r -n1 umount -l
echo "left: $(pgrep -c --ns $$ --nslist mnt -f "[c]ontainerd-shim-gcr-v2" || echo 0) shims, $(crictl pods -q 2>/dev/null | wc -l) pods"
