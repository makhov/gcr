# The page the game is played in: noVNC's client, behind websockify,
# which connects to the doom Service wherever its pod runs; and the mover,
# which moves the pod.
FROM debian:bookworm-slim
RUN apt-get update && \
	apt-get install -y --no-install-recommends novnc websockify python3 && \
	rm -rf /var/lib/apt/lists/* && \
	cp -r /usr/share/novnc/. /srv/ && rm -f /srv/index.html
COPY index.html mover.py arcade.sh /srv/
EXPOSE 6080
CMD ["/srv/arcade.sh"]
