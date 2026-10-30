#!/bin/bash
# A fork from a registry, as on a node that has never seen the snapshot: a
# Python process holding 1 GiB of random data is snapshotted, pushed in the
# nydus format to a registry on this host, removed, and forked after a
# plain pull, a lazy one, and a lazy one with lazy restore. Prints, for
# each, how long until the fork runs and what it read from the registry
# (docs/snapshot-registry.md#lazy-pulls). Run as root, with gcr, nydus and
# about 4 GiB free; sets vm.unprivileged_userfaultfd=1 for lazy restore.
set -u
NS=gcr-bench; IMG=docker.io/library/python:3.12-alpine; REG=127.0.0.1:5999
W=/var/lib/gcr/bench; rm -rf $W; mkdir -p $W; chmod 700 $W
now() { date +%s.%N; }
since() { echo "$(now) - $1" | bc | xargs printf '%.2f'; }
cleanup() { for id in $(ctr -n $NS c ls -q); do ctr -n $NS t kill -s KILL $id >/dev/null 2>&1; sleep 0.2; ctr -n $NS t rm -f $id >/dev/null 2>&1; ctr -n $NS c rm $id >/dev/null 2>&1; done; }
cleanup
sysctl -qw vm.unprivileged_userfaultfd=1
ctr -n $NS i pull -q $IMG >/dev/null 2>&1 || ctr -n $NS i pull $IMG >/dev/null
ctr -n $NS i pull docker.io/library/registry:3 >/dev/null 2>&1
ctr -n $NS run -d --null-io --net-host --env REGISTRY_HTTP_ADDR=$REG --env OTEL_TRACES_EXPORTER=none docker.io/library/registry:3 reg >/dev/null
until curl -sf http://$REG/v2/ >/dev/null; do sleep 0.2; done

PROG='
import os, time
data = bytearray(1 << 30)
for i in range(0, len(data), 64 << 20):
    data[i:i + (64 << 20)] = os.urandom(64 << 20)
open("/ready", "w").write("1")
n = 0
while True:
    s = sum(data[0:4 << 20:4096])          # 4 MiB of the 1 GiB touched
    n += 1
    open("/hb", "w").write("%d %d %f" % (n, s, time.time()))
    time.sleep(0.05)
'
ctr -n $NS run -d --runtime io.containerd.gcr.v2 $IMG parent python3 -c "$PROG" >/dev/null
until [ -e /run/containerd/io.containerd.runtime.v2.task/$NS/parent/rootfs/ready ]; do sleep 0.2; done
t=$(now); gcr -n $NS snapshot parent $W/snap; echo "snapshot: $(since $t)s, $(du -sh $W/snap/images | cut -f1) images"
t=$(now); gcr snapshot-push -format nydus $W/snap $REG/bench/py:1 >/dev/null; echo "push (nydus): $(since $t)s"
echo "registry blobs: $(curl -s http://$REG/v2/bench/py/manifests/1 -H 'Accept: application/vnd.oci.image.manifest.v1+json' | python3 -c 'import json,sys; m=json.load(sys.stdin); print(sum(l["size"] for l in m["layers"])//(1<<20), "MiB")')"
cleanup_parent() { ctr -n $NS t kill -s KILL parent >/dev/null 2>&1; sleep 0.3; ctr -n $NS t rm -f parent >/dev/null 2>&1; ctr -n $NS c rm parent >/dev/null 2>&1; gcr snapshot-rm $W/snap; }
cleanup_parent
sync; echo 3 > /proc/sys/vm/drop_caches

fork() { # name dir [annotations...] -> seconds until the fork's first heartbeat
  local name=$1 dir=$2; shift 2; local t=$(now)
  ctr -n $NS run -d --runtime io.containerd.gcr.v2 --annotation gcr.dev/fork-from=$dir "$@" $IMG $name true >/dev/null
  local hb=/run/containerd/io.containerd.runtime.v2.task/$NS/$name/rootfs/hb
  local deadline=$(( ${t%.*} + 120 ))
  until [ -e $hb ] && [ "$(cut -d' ' -f3 $hb 2>/dev/null | cut -d. -f1)" -ge "${t%.*}" ] 2>/dev/null; do
    sleep 0.01
    if [ $(date +%s) -gt $deadline ]; then echo "TIMEOUT($(ctr -n $NS t ls | grep $name | awk '{print $3}'))"; return; fi
  done
  since $t
}
run() { # label pull-flags fork-annotations
  local label=$1 flags=$2 ann=$3 d=$W/$4
  sync; echo 3 > /proc/sys/vm/drop_caches
  local t=$(now); gcr snapshot-pull $flags -store $W/layers-$4 $REG/bench/py:1 $d >/dev/null; local p=$(since $t)
  local f; f=$(fork f-$4 $d $ann)
  local fetched
  local fetched later=""
  if [ -d $d/gcr-lazy ]; then
    fetched=$(du -sck $d/gcr-lazy/*/cache $W/layers-$4/*/gcr-lazy/*/cache 2>/dev/null | tail -1 | cut -f1)
    sleep 10
    later=" (10s later: $(( $(du -sck $d/gcr-lazy/*/cache $W/layers-$4/*/gcr-lazy/*/cache 2>/dev/null | tail -1 | cut -f1) >> 10 )) MiB)"
  else fetched=$(du -sck $d/images $d/upper | tail -1 | cut -f1); fi
  printf '%-26s pull %5ss  fork running %5ss  total %5ss  read from registry %5d MiB%s\n' "$label" $p $f $(echo "$p + $f" | bc) $((fetched >> 10)) "$later"
}
run "eager pull"                         ""      ""                                   eager
run "lazy pull, restore"                 "-lazy" ""                                   lazy
run "lazy pull, lazy restore"            "-lazy" "--annotation gcr.dev/lazy-restore=true" lazylr
cleanup
for d in eager lazy lazylr; do gcr snapshot-rm $W/$d >/dev/null 2>&1 || true; done
rm -rf $W
