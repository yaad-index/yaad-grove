#!/bin/sh
# Fetches the LadybugDB C library (headers and liblbug.so) into <lib-dir>, for
# the ladybug-tagged build (ADR 0019). Given <home-dir> too, it also places the
# vector and FTS extensions under <home-dir>/.lbdb, where LOAD EXTENSION finds
# them, so nothing needs INSTALL from the network. Every release and file is
# pinned and checked against its SHA-256 before use; all of them change only by
# editing this file (#213, #215).
#
#   fetch-liblbug.sh <lib-dir> [<home-dir>]
#   fetch-liblbug.sh --check <home-dir>
#
# --check verifies that <home-dir>/.lbdb holds exactly the pinned extension
# files, unchanged: after a test run, a difference means an INSTALL happened.
set -eu

VERSION=0.21.2
ARCHIVE=liblbug-linux-x86_64.tar.gz
SHA256=f3de0f9be86fffd0919bc7a7f65699d1b099d0a1df7115142fa1606ba83c94c4

# The extension directory the pinned library loads from: it reads
# ~/.lbdb/extension/<EXT_VERSION>/<EXT_PLATFORM>/<name>/lib<name>.lbug_extension.
EXT_VERSION=0.21.0
EXT_PLATFORM=linux_amd64
EXT_FTS_SHA256=742f2756c2f80bcd1886b6ee0446483038cff0ecf6cf47f698bc6fe855cbaef6
EXT_VECTOR_SHA256=571726cc2ea0d202df1909ae5a3b0528c9dea3ea8d40be6c16b310ec14aa8821

usage() {
	echo "usage: fetch-liblbug.sh <lib-dir> [<home-dir>] | --check <home-dir>" >&2
	exit 2
}

# ext_sums writes the sha256sum lines of the pinned extension files under home $1.
ext_sums() {
	dir="$1/.lbdb/extension/$EXT_VERSION/$EXT_PLATFORM"
	echo "$EXT_FTS_SHA256  $dir/fts/libfts.lbug_extension"
	echo "$EXT_VECTOR_SHA256  $dir/vector/libvector.lbug_extension"
}

if [ "${1:-}" = "--check" ]; then
	[ $# -eq 2 ] || usage
	ext_sums "$2" | sha256sum -c -
	found="$(find "$2/.lbdb" -type f | sort)"
	want="$(ext_sums "$2" | cut -d' ' -f3- | sort)"
	if [ "$found" != "$want" ]; then
		printf 'fetch-liblbug.sh: %s/.lbdb holds files that are not pinned:\n%s\n' "$2" "$found" >&2
		exit 1
	fi
	exit 0
fi

[ $# -ge 1 ] && [ $# -le 2 ] || usage
if [ "$(uname -s)-$(uname -m)" != "Linux-x86_64" ]; then
	echo "fetch-liblbug.sh: only Linux x86_64 is pinned" >&2
	exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL --retry 3 -o "$tmp/$ARCHIVE" \
	"https://github.com/LadybugDB/ladybug/releases/download/v${VERSION}/${ARCHIVE}"
echo "${SHA256}  $tmp/$ARCHIVE" | sha256sum -c -
mkdir -p "$1"
tar xzf "$tmp/$ARCHIVE" -C "$1"

if [ $# -eq 2 ]; then
	for name in fts vector; do
		dir="$tmp/home/.lbdb/extension/$EXT_VERSION/$EXT_PLATFORM/$name"
		mkdir -p "$dir"
		curl -fsSL --retry 3 -o "$dir/lib${name}.lbug_extension" \
			"https://extension.ladybugdb.com/v${EXT_VERSION}/${EXT_PLATFORM}/${name}/lib${name}.lbug_extension"
	done
	ext_sums "$tmp/home" | sha256sum -c -
	mkdir -p "$2/.lbdb"
	cp -R "$tmp/home/.lbdb/." "$2/.lbdb/"
fi
