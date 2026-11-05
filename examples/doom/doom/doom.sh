#!/bin/sh
# X without MIT-SHM, and x11vnc without shared memory: nothing is kept in
# System V shared memory, which belongs to the pod's IPC namespace, not the
# container, and would not move with it.
set -eu
export DISPLAY=:0 SDL_AUDIODRIVER=dummy HOME=/tmp
Xvfb :0 -screen 0 640x400x24 -nolisten tcp -extension MIT-SHM &
while [ ! -e /tmp/.X11-unix/X0 ]; do sleep 0.1; done
x11vnc -display :0 -rfbport 5900 -forever -shared -nopw -noshm -quiet &
# As PID 1, the shell stops on SIGTERM only between commands: the game
# runs in the background, waited for.
trap 'kill $(jobs -p) 2>/dev/null; exit 0' TERM INT
# Quitting the game starts a new one, rather than the container over.
while :; do
	/usr/games/chocolate-doom -iwad /usr/share/games/doom/freedoom1.wad \
		-window -geometry 640x400 -nosound -nograbmouse &
	wait $! || sleep 1
done
