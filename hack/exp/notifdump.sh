#!/bin/bash
# Runs the notifdump experiment. Needs root and criu.
set -u
cd "$(dirname "$0")"
gcc -O1 -o /tmp/notifdump notifdump.c || exit 1
pkill -f /tmp/notifdump; rm -rf /tmp/nd /tmp/nd-img; mkdir -p /tmp/nd-img

/tmp/notifdump > /tmp/nd-super.log 2>&1 &
sleep 1.5
child=$(sed -n 's/child=\([0-9]*\).*/\1/p' /tmp/nd-super.log)
echo "child=$child; log before hold:"; tail -2 /tmp/nd/child.log

echo "== hold the next mkdir"
touch /tmp/nd/hold
for _ in $(seq 50); do [ -f /tmp/nd/held ] && break; sleep 0.1; done
echo "held pid=$(cat /tmp/nd/held) count=$(cat /tmp/nd/count)"

echo "== criu dump while held"
criu dump -t "$child" -D /tmp/nd-img -o dump.log -v4 --shell-job
echo "dump exit=$?"; grep -iE "error|seccomp|notif" /tmp/nd-img/dump.log | tail -15

echo "== release supervisor and restore"
touch /tmp/nd/go
sleep 0.5
criu restore -D /tmp/nd-img -o restore.log -v4 --shell-job -d
echo "restore exit=$?"; grep -iE "error|seccomp|notif" /tmp/nd-img/restore.log | tail -15
sleep 1.5
echo "== log after restore:"; tail -6 /tmp/nd/child.log
pkill -f /tmp/notifdump
