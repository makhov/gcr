#!/bin/sh
# Installs gcr from its GitHub releases:
#
#   curl -sSfL https://raw.githubusercontent.com/makhov/gcr/main/install.sh | sudo sh
#
# downloads the gcr binary for this machine, checks it against the
# release's SHA256SUMS, and runs `gcr setup`, which installs gcr and its
# shim and registers the runtime with containerd. Arguments go to gcr
# setup (docs/cli.md#gcr-setup):
#
#   curl -sSfL .../install.sh | sudo sh -s -- -docker     # Docker too
#
# With kubectl-gcr as the first argument it installs the kubectl plugin
# instead, on Linux or macOS:
#
#   curl -sSfL .../install.sh | sh -s -- kubectl-gcr
#
# Environment:
#   GCR_VERSION   the release to install, such as v0.1.8 (default: the latest)
#   GCR_BIN_DIR   where the kubectl plugin goes (default: /usr/local/bin)
#   GCR_NO_SETUP  set to 1: only download gcr, to ./gcr, without gcr setup
set -eu

repo=https://github.com/makhov/gcr

fail() {
	echo "gcr install: $*" >&2
	exit 1
}

have() { command -v "$1" >/dev/null 2>&1; }

# fetch URL FILE
fetch() {
	if have curl; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif have wget; then
		wget -qO "$2" "$1"
	else
		fail "needs curl or wget"
	fi
}

# The latest release's tag, from where releases/latest redirects to: no
# API call, so no rate limit.
latest() {
	if have curl; then
		url=$(curl -fsSLo /dev/null -w '%{url_effective}' "$repo/releases/latest")
	else
		url=$(wget -S --spider "$repo/releases/latest" 2>&1 | sed -n 's/^ *[Ll]ocation: *\([^ ]*\).*/\1/p' | tail -1)
	fi
	tag=${url##*/}
	case $tag in
	v*) echo "$tag" ;;
	*) fail "could not find the latest release (got $url): set GCR_VERSION" ;;
	esac
}

sha256() {
	if have sha256sum; then
		sha256sum "$1" | cut -d' ' -f1
	elif have shasum; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		fail "needs sha256sum or shasum to check the download"
	fi
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "no release for $(uname -m): gcr is built for amd64 and arm64" ;;
esac

what=gcr
if [ "${1:-}" = kubectl-gcr ]; then
	what=kubectl-gcr
	shift
	case $os in
	linux | darwin) ;;
	*) fail "no kubectl-gcr for $os: it is built for Linux and macOS" ;;
	esac
else
	[ "$os" = linux ] || fail "gcr runs on Linux only; on $os, try it in a VM (docs/install.md#a-vm-on-your-laptop), or install the kubectl plugin: sh -s -- kubectl-gcr"
	if [ "${GCR_NO_SETUP:-}" != 1 ] && [ "$(id -u)" != 0 ]; then
		fail "gcr setup needs root: pipe this to sudo sh"
	fi
fi

version=${GCR_VERSION:-$(latest)}
asset=$what-$version-$os-$arch
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $asset"
fetch "$repo/releases/download/$version/$asset" "$tmp/$what" ||
	fail "could not download $repo/releases/download/$version/$asset"
fetch "$repo/releases/download/$version/SHA256SUMS" "$tmp/SHA256SUMS" ||
	fail "could not download $version's SHA256SUMS"
want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || fail "$asset is not in $version's SHA256SUMS"
[ "$(sha256 "$tmp/$what")" = "$want" ] || fail "$asset does not match its checksum in SHA256SUMS"
chmod 0755 "$tmp/$what"

if [ "$what" = kubectl-gcr ]; then
	dir=${GCR_BIN_DIR:-/usr/local/bin}
	mkdir -p "$dir" 2>/dev/null || true
	if [ -w "$dir" ]; then
		mv "$tmp/kubectl-gcr" "$dir/kubectl-gcr"
	elif have sudo; then
		echo "Installing to $dir with sudo"
		sudo mkdir -p "$dir"
		sudo mv "$tmp/kubectl-gcr" "$dir/kubectl-gcr"
	else
		fail "cannot write to $dir: set GCR_BIN_DIR, or run as root"
	fi
	echo "Installed kubectl-gcr $version to $dir: try kubectl gcr"
	exit 0
fi

if [ "${GCR_NO_SETUP:-}" = 1 ]; then
	mv "$tmp/gcr" ./gcr
	echo "Downloaded gcr $version to ./gcr"
	exit 0
fi

# gcr setup copies itself to its -bin-dir (/usr/local/bin), so the
# download can go once it is done.
"$tmp/gcr" setup "$@"
