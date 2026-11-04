# The image gcr.yaml's installer DaemonSet and gcr-operator.yaml's operator
# run: a release's gcr (dist/, which carries the shim and gcr's CRIU), the
# operator, and busybox, for the installer's sh and nsenter (which the
# operator's migration Jobs use too). `make image` builds it for amd64 and
# arm64.
FROM busybox:1.37
ARG TARGETARCH
ARG VERSION
# --chmod: dist/ comes through GitHub Actions artifacts in a release, which
# keep no file modes, and a binary without its x bit cannot start.
COPY --chmod=0755 dist/gcr-${VERSION}-linux-${TARGETARCH} /gcr
COPY --chmod=0755 dist/gcr-operator-${VERSION}-linux-${TARGETARCH} /gcr-operator
COPY deploy/install.sh /install.sh
ENTRYPOINT ["/install.sh"]
