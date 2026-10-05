#!/bin/sh
# Fetches the LadybugDB C library (headers and liblbug.so) into the directory
# given as the only argument, for the ladybug-tagged build (ADR 0019). The
# release is pinned and the archive is checked against its SHA-256 before it is
# unpacked; both change only by editing this file (#213).
set -eu

VERSION=0.21.2
ARCHIVE=liblbug-linux-x86_64.tar.gz
SHA256=f3de0f9be86fffd0919bc7a7f65699d1b099d0a1df7115142fa1606ba83c94c4

target="${1:?usage: fetch-liblbug.sh <target-dir>}"
if [ "$(uname -s)-$(uname -m)" != "Linux-x86_64" ]; then
	echo "fetch-liblbug.sh: only Linux x86_64 is pinned" >&2
	exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --retry 3 -o "$tmp/$ARCHIVE" \
	"https://github.com/LadybugDB/ladybug/releases/download/v${VERSION}/${ARCHIVE}"
echo "${SHA256}  $tmp/$ARCHIVE" | sha256sum -c -
mkdir -p "$target"
tar xzf "$tmp/$ARCHIVE" -C "$target"
