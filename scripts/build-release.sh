#!/bin/sh
# Builds the release binaries and SHA256SUMS: scripts/build-release.sh VERSION OUTDIR
# Used by .github/workflows/release.yml and by the launcher tests.
set -eu
version=$1
out=$2
mkdir -p "$out"
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
	os=${target%/*}
	arch=${target#*/}
	ext=""
	[ "$os" = windows ] && ext=".exe"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$version" \
		-o "$out/perritite-$os-$arch$ext" .
done
cd "$out"
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum perritite-* >SHA256SUMS
else
	shasum -a 256 perritite-* >SHA256SUMS
fi
