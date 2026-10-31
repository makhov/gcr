# The image gcr.yaml's installer DaemonSet and gcr-operator.yaml's operator
# run: a release's gcr (dist/, which carries the shim and gcr's CRIU), the
# operator, and busybox, for the installer's sh and nsenter (which the
# operator's migration Jobs use too). `make image` builds it for amd64 and
# arm64.
FROM busybox:1.37
ARG TARGETARCH
ARG VERSION
COPY dist/gcr-${VERSION}-linux-${TARGETARCH} /gcr
COPY dist/gcr-operator-${VERSION}-linux-${TARGETARCH} /gcr-operator
COPY deploy/install.sh /install.sh
ENTRYPOINT ["/install.sh"]
