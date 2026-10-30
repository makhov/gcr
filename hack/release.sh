#!/bin/bash
# Builds a release into dist/: for each architecture one file,
# gcr-VERSION-linux-ARCH, the gcr command carrying the shim, gcr's CRIU and
# CRIU's CUDA plugin (internal/payload) for `gcr setup` to install;
# gcr.yaml, the installer DaemonSet with this release's image; and
# SHA256SUMS. The Go binaries are static; CRIU, built in an Ubuntu 22.04
# container (hack/criu-build.sh), needs glibc 2.35 or later. Runs on a
# Linux host with Docker that can build Go with cgo for each architecture
# (docs/development.md#releases).
#
# CRIU is makhov/criu at the tag in hack/criu-version, or, to try a change,
# the tree in CRIU_SRC.
#
#   VERSION=v0.1.0 hack/release.sh amd64 arm64
set -euo pipefail

VERSION=${VERSION:?set VERSION}
CRIU_VERSION=$(cat hack/criu-version)
CRIU_IMAGE=${CRIU_IMAGE:-ubuntu:22.04}
DOCKER=${DOCKER:-docker}
HOST=$(go env GOARCH)
REVISION=$(git rev-parse HEAD)
VERSION_PKG=github.com/containerd/containerd/v2/version
PAYLOAD=internal/payload/files

triplet() { case $1 in amd64) echo x86_64-linux-gnu ;; arm64) echo aarch64-linux-gnu ;; esac; }

gobuild() { # ARCH OUT PKG TAGS
	local t cc=gcc
	t=$(triplet "$1")
	[ "$1" = "$HOST" ] || cc=$t-gcc
	CC=$cc GOOS=linux GOARCH=$1 CGO_ENABLED=1 PKG_CONFIG_LIBDIR=/usr/lib/$t/pkgconfig \
		go build -trimpath -tags "seccomp netgo osusergo $4" \
		-ldflags "-s -w -X $VERSION_PKG.Version=$VERSION -X $VERSION_PKG.Revision=$REVISION -linkmode external -extldflags -static" \
		-o "$2" "$3"
}

rm -rf dist && mkdir -p dist
work=$(mktemp -d)
trap 'rm -rf "$work"; find "$PAYLOAD" -name "*.zst" -delete' EXIT

if [ -n "${CRIU_SRC:-}" ]; then
	CRIU_SRC=$(cd "$CRIU_SRC" && pwd)
else
	CRIU_SRC=$work/criu-src
	git -c advice.detachedHead=false clone -q --depth 1 --branch "$CRIU_VERSION" https://github.com/makhov/criu.git "$CRIU_SRC"
fi

for arch in "$@"; do
	find "$PAYLOAD" -name "*.zst" -delete
	gobuild "$arch" "$work/containerd-shim-gcr-v2" ./cmd/containerd-shim-gcr-v2 ""
	mkdir -p "$work/criu-$arch"
	$DOCKER run --rm -v "$CRIU_SRC:/src:ro" -v "$work/criu-$arch:/out" -v "$PWD/hack:/hack:ro" \
		"$CRIU_IMAGE" /hack/criu-build.sh "$arch" /src /out
	{
		echo "gcr $VERSION ($REVISION)"
		echo "criu $(git -C "$CRIU_SRC" describe --tags --always --dirty), built on $CRIU_IMAGE"
	} >"$work/VERSIONS"
	for f in "$work/containerd-shim-gcr-v2" "$work/criu-$arch/criu" "$work/criu-$arch/cuda_plugin.so" "$work/VERSIONS"; do
		zstd -q -19 -T0 "$f" -o "$PAYLOAD/$(basename "$f").zst"
	done
	gobuild "$arch" "dist/gcr-$VERSION-linux-$arch" ./cmd/gcr gcr_payload
done
# The installer DaemonSet for this release's image.
sed "s#image: ghcr.io/makhov/gcr:.*#image: ghcr.io/makhov/gcr:$VERSION#" deploy/gcr.yaml >dist/gcr.yaml
(cd dist && sha256sum gcr-* gcr.yaml >SHA256SUMS)
