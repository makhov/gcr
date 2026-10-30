# The installer image gcr.yaml's DaemonSet runs: a release's gcr (dist/,
# which carries the shim and gcr's CRIU) and busybox, for the script's sh
# and nsenter. `make image` builds it for amd64 and arm64.
FROM busybox:1.37
ARG TARGETARCH
ARG VERSION
COPY dist/gcr-${VERSION}-linux-${TARGETARCH} /gcr
COPY deploy/install.sh /install.sh
ENTRYPOINT ["/install.sh"]
