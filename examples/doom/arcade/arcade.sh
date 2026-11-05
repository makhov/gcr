#!/bin/sh
# GAME is the VNC server, host:port; INTERVAL, seconds between moves (0:
# none).
#
# websockify connects to what /tmp/targets names for the page's token,
# reread on each connection: GAME to start with, and then, from the mover,
# the game's pod itself. A move reaches the pod's address before the
# Service catches up.
set -eu
echo '{}' >/srv/status.json
echo "doom: ${GAME:-doom:5900}" >/tmp/targets
if [ "${INTERVAL:-20}" != 0 ]; then
	python3 /srv/mover.py &
fi
exec websockify --web /srv --token-plugin TokenFile --token-source /tmp/targets 6080
