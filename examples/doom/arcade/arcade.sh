#!/bin/sh
# GAME is the VNC server, host:port; INTERVAL, seconds between moves (0:
# none).
set -eu
echo '{}' >/srv/status.json
if [ "${INTERVAL:-20}" != 0 ]; then
	python3 /srv/mover.py &
fi
exec websockify --web /srv 6080 "${GAME:-doom:5900}"
