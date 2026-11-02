#!/bin/sh
# Installs the gcr binary from gcr's GitHub releases:
#
#   curl -sSfL https://raw.githubusercontent.com/makhov/gcr/main/install.sh | sh
#
# picks the release (the latest, or GCR_VERSION) and this machine's build,
# checks it against the release's SHA256SUMS, and installs it into
# /usr/local/bin, with sudo if it has to. It changes nothing else: `sudo
# gcr setup` then registers gcr with containerd (docs/install.md).
#
# With kubectl-gcr as the argument it installs the kubectl plugin
# instead, on Linux or macOS:
#
#   curl -sSfL .../install.sh | sh -s -- kubectl-gcr
#
# Environment:
#   GCR_VERSION   the release to install, such as v0.1.8 (default: the latest)
#   GCR_BIN_DIR   where it goes (default: /usr/local/bin)
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

case ${1:-gcr} in
gcr)
	what=gcr
	[ "$os" = linux ] || fail "gcr runs on Linux only; on $os, try it in a VM (docs/install.md#a-vm-on-your-laptop), or install the kubectl plugin: sh -s -- kubectl-gcr"
	;;
kubectl-gcr)
	what=kubectl-gcr
	case $os in
	linux | darwin) ;;
	*) fail "no kubectl-gcr for $os: it is built for Linux and macOS" ;;
	esac
	;;
-*) fail "$1 is gcr setup's: install gcr first, then run sudo gcr setup $*" ;;
*) fail "unknown argument $1: give none for gcr, or kubectl-gcr for the kubectl plugin" ;;
esac

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

# Into the directory first, then renamed over the old one: a gcr that is
# running goes on with its own copy.
dir=${GCR_BIN_DIR:-/usr/local/bin}
sudo=
mkdir -p "$dir" 2>/dev/null || true
if [ ! -w "$dir" ]; then
	have sudo || fail "cannot write to $dir: run as root, or set GCR_BIN_DIR"
	echo "Installing into $dir with sudo"
	sudo=sudo
	$sudo mkdir -p "$dir"
fi
$sudo cp "$tmp/$what" "$dir/.$what.new"
$sudo mv "$dir/.$what.new" "$dir/$what"
echo "Installed $what $version as $dir/$what"

if [ "$what" = gcr ]; then
	echo "Next: sudo gcr setup (add -docker to register with Docker too)"
fi
