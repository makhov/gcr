# Chocolate Doom playing Freedoom on a virtual X display, served over VNC.
FROM debian:bookworm-slim
RUN apt-get update && \
	apt-get install -y --no-install-recommends chocolate-doom freedoom xvfb x11vnc && \
	rm -rf /var/lib/apt/lists/*
COPY doom.sh /doom.sh
EXPOSE 5900
CMD ["/doom.sh"]
