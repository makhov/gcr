#!/bin/bash
# Builds gcr's CRIU and its CUDA plugin for ARCH (amd64, arm64) into OUT,
# from the source tree in SRC: libraries linked in statically, glibc
# dynamically (makhov/criu's STATIC=libs), so they run on a glibc as old as
# this host's. Run by hack/release.sh in an Ubuntu 22.04 container
# (glibc 2.35), as root; it installs the build dependencies itself.
#
#   hack/criu-build.sh ARCH SRC OUT
set -euo pipefail
ARCH=$1 SRC=$2 OUT=$3
HOST=$(dpkg --print-architecture)
export DEBIAN_FRONTEND=noninteractive

deps=(libprotobuf-c-dev libnl-3-dev libnet1-dev libcap-dev libbsd-dev libbpf-dev uuid-dev
	libelf-dev zlib1g-dev libzstd-dev libmd-dev libprotobuf-dev)
cross=()
if [ "$ARCH" != "$HOST" ]; then
	# Ubuntu serves amd64 from archive and arm64 from ports.
	. /etc/os-release
	url() { [ "$1" = amd64 ] && echo http://archive.ubuntu.com/ubuntu || echo http://ports.ubuntu.com/ubuntu-ports; }
	sed -i "s#^deb #deb [arch=$HOST] #" /etc/apt/sources.list
	for s in "$VERSION_CODENAME" "$VERSION_CODENAME-updates" "$VERSION_CODENAME-security"; do
		echo "deb [arch=$ARCH] $(url "$ARCH") $s main universe"
	done >/etc/apt/sources.list.d/cross.list
	dpkg --add-architecture "$ARCH"
	t=$([ "$ARCH" = amd64 ] && echo x86_64-linux-gnu || echo aarch64-linux-gnu)
	criu_arch=$([ "$ARCH" = amd64 ] && echo x86 || echo aarch64)
	cross=(ARCH="$criu_arch" CROSS_COMPILE="$t-" CC="$t-gcc")
	export PKG_CONFIG_LIBDIR=/usr/lib/$t/pkgconfig:/usr/share/pkgconfig:/usr/lib/$(dpkg-architecture -qDEB_HOST_MULTIARCH)/pkgconfig
	# gcc and libc6-dev for compel-host, which runs here.
	pkgs=(gcc libc6-dev "gcc-${t//_/-}" "libc6-dev-$ARCH-cross" "${deps[@]/%/:$ARCH}")
else
	pkgs=(gcc libc6-dev "${deps[@]}")
fi
apt-get update -qq
apt-get install -y -qq --no-install-recommends make pkg-config protobuf-c-compiler protobuf-compiler \
	python3 git ca-certificates "${pkgs[@]}" >/dev/null

build=$(mktemp -d)
cp -r "$SRC/." "$build" 2>/dev/null || true # sockets its tests leave
make -s -C "$build" mrproper >/dev/null
make -s -C "$build" -j"$(nproc)" criu cuda_plugin "${cross[@]}" STATIC=libs NO_GNUTLS=1 NO_NFTABLES=1 \
	2> >(grep -v "warning: Using\|in function\|RWX permissions" >&2)
mkdir -p "$OUT"
install -m 0755 "$build/criu/criu" "$OUT/criu"
install -m 0755 "$build/plugins/cuda/cuda_plugin.so" "$OUT/cuda_plugin.so"
