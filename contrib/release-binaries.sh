#!/bin/sh
# Builds the desktop binary for every Linux architecture and packs one
# archive each, plus SHA256SUMS, for a GitHub release:
#
#	contrib/release-binaries.sh 0.6.0 [OUTDIR]
#
# Each archive holds the binary and what a manual install needs (the
# systemd unit, the settings menu entry, file manager actions and the DMS
# plugin). Builds are static and stripped, and the archives reproducible:
# fixed file order, owner and timestamps (the commit's).
set -eu

version=${1:?usage: $0 VERSION [OUTDIR]}
out=${2:-dist}
cd "$(dirname "$0")/.."
mkdir -p "$out"
out=$(cd "$out" && pwd)
stamp=$(git log -1 --format=%ct)

# GOARCH:GOARM name
for target in amd64::x86_64 arm64::aarch64 arm:7:armv7 386::i686 riscv64::riscv64; do
	arch=${target%%:*}
	rest=${target#*:}
	goarm=${rest%%:*}
	name=${rest#*:}
	dir=$(mktemp -d)
	pkg="pocketlink-$version-linux-$name"
	mkdir -p "$dir/$pkg"

	GOOS=linux GOARCH=$arch GOARM=$goarm CGO_ENABLED=0 \
		go build -trimpath -ldflags="-s -w -buildid=" -o "$dir/$pkg/pocketlink" ./cmd/pocketlink
	cp README.md LICENSE contrib/pocketlink.service contrib/pocketlink-settings.desktop "$dir/$pkg/"
	cp -r contrib/filemanager "$dir/$pkg/"
	mkdir -p "$dir/$pkg/dms"
	cp -r contrib/dms/pocketlink "$dir/$pkg/dms/"

	tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$stamp" \
		-C "$dir" -cf - "$pkg" | gzip -n9 > "$out/$pkg.tar.gz"
	rm -rf "$dir"
	echo "$out/$pkg.tar.gz"
done

(cd "$out" && sha256sum pocketlink-"$version"-linux-*.tar.gz > SHA256SUMS)
echo "$out/SHA256SUMS"
