#!/bin/bash
# Builds release archives into dist/: for each architecture, static
# binaries of the shim, gcr and CRIU (gcr's fork, from CRIU_SRC, built
# without plugins), with SHA256SUMS. Runs on a Linux host that can build
# with cgo for each, natively or across (docs/development.md#releases).
#
#   VERSION=v0.1.0 CRIU_SRC=../criu hack/release.sh amd64 arm64
set -euo pipefail

VERSION=${VERSION:?set VERSION}
CRIU_SRC=$(cd "${CRIU_SRC:-../criu}" && pwd)
ARCHES=("$@")
HOST=$(go env GOARCH)
REVISION=$(git rev-parse HEAD)
VERSION_PKG=github.com/containerd/containerd/v2/version

triplet() { case $1 in amd64) echo x86_64-linux-gnu ;; arm64) echo aarch64-linux-gnu ;; esac; }
criu_arch() { case $1 in amd64) echo x86 ;; arm64) echo aarch64 ;; esac; }

rm -rf dist && mkdir -p dist
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for arch in "${ARCHES[@]}"; do
	t=$(triplet "$arch")
	cc=gcc
	cross=()
	if [ "$arch" != "$HOST" ]; then
		cc=$t-gcc
		cross=(ARCH="$(criu_arch "$arch")" CROSS_COMPILE="$t-" CC="$cc")
	fi
	out=dist/gcr-$VERSION-linux-$arch
	mkdir -p "$out"

	for cmd in containerd-shim-gcr-v2 gcr; do
		CC=$cc GOOS=linux GOARCH=$arch CGO_ENABLED=1 PKG_CONFIG_LIBDIR=/usr/lib/$t/pkgconfig \
			go build -trimpath -tags "seccomp netgo osusergo" \
			-ldflags "-s -w -X $VERSION_PKG.Version=$VERSION -X $VERSION_PKG.Revision=$REVISION -linkmode external -extldflags -static" \
			-o "$out/$cmd" "./cmd/$cmd"
	done

	# CRIU in a copy of its tree, so its own build products stay its own.
	# pkg-config falls back to the host's directory for what is the same
	# everywhere (protobuf's .proto files).
	src=$work/criu-$arch
	cp -r "$CRIU_SRC" "$src" 2>/dev/null || true # sockets left by its tests
	make -s -C "$src" mrproper >/dev/null
	PKG_CONFIG_LIBDIR=/usr/lib/$t/pkgconfig:/usr/share/pkgconfig:/usr/lib/$(triplet "$HOST")/pkgconfig \
		make -s -C "$src" -j"$(nproc)" criu "${cross[@]}" STATIC=1 NO_GNUTLS=1 NO_NFTABLES=1 \
		2> >(grep -v "warning: Using\|in function" >&2)
	install -m 0755 "$src/criu/criu" "$out/criu"

	cp README.md "$out/"
	{
		echo "gcr $VERSION ($REVISION)"
		echo "criu $(git -C "$CRIU_SRC" describe --always --dirty) (static, no plugins)"
	} >"$out/VERSIONS"
	tar -C dist -czf "$out.tar.gz" "$(basename "$out")"
	rm -rf "$out"
done
(cd dist && sha256sum ./*.tar.gz | sed 's# \./# #' >SHA256SUMS)
