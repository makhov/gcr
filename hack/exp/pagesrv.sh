#!/bin/bash
# Experiment: two-round pre-copy migration with CRIU page servers, all on
# one host. Shows how the round directories chain on the target side.
set -u
T=/tmp/pm S=/tmp/ps
rm -rf $T $S; mkdir -p $T/1 $T/2 $S/1 $S/2

setsid bash -c 'i=0; buf=$(head -c 2000000 /dev/urandom | base64); while true; do i=$((i+1)); echo $i > /tmp/pm-count; sleep 0.1; done' </dev/null >/dev/null 2>&1 &
pid=$!
sleep 1

criu page-server -D $T/1 --port 7001 -o ps.log -v4 &
ps1=$!; sleep 0.3
criu pre-dump -t $pid -D $S/1 --page-server --address 127.0.0.1 --port 7001 --shell-job --track-mem -o pre.log -v4
echo "predump=$?"; wait $ps1

criu page-server -D $T/2 --port 7002 --prev-images-dir ../1 -o ps.log -v4 &
ps2=$!; sleep 0.3
criu dump -t $pid -D $S/2 --prev-images-dir ../1 --track-mem --page-server --address 127.0.0.1 --port 7002 --shell-job -o dump.log -v4
echo "dump=$?"; wait $ps2

echo "target 1: $(ls $T/1 | tr '\n' ' ')"
echo "target 2: $(ls -l $T/2 | grep -E 'parent|pagemap|pages' | awk '{print $9,$10,$11}' | tr '\n' ' ')"
echo "source 2: $(ls -l $S/2 | grep -E 'parent|pagemap|pages|core' | awk '{print $9,$10,$11}' | tr '\n' ' ')"

c=$(cat /tmp/pm-count)
cp -a $S/2/. $T/2/
criu restore -D $T/2 --shell-job -d -o restore.log -v4
echo "restore=$? count_at_dump=$c"
sleep 0.5
echo "count after restore: $(cat /tmp/pm-count)"
grep -i error $T/2/restore.log | head -5
kill "$(pgrep -n -f 'pm-count; sleep')" 2>/dev/null
