#!/bin/sh
# Builds the release binaries and SHA256SUMS: scripts/build-release.sh VERSION OUTDIR
#
# Builds are reproducible: the exact Go toolchain pinned in go.mod, no VCS
# stamping, trimmed paths. So the same commit gives byte-identical binaries
# on any machine, which is what lets the plugin ship launcher/SHA256SUMS and
# the release workflow refuse to publish anything that doesn't match it.
set -eu
version=$1
out=$2
cd "$(dirname "$0")/.."
toolchain=$(sed -n 's/^toolchain //p' go.mod)
[ -n "$toolchain" ] || { echo "go.mod has no toolchain line" >&2; exit 1; }
export GOTOOLCHAIN="$toolchain" CGO_ENABLED=0
mkdir -p "$out"
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
	os=${target%/*}
	arch=${target#*/}
	ext=""
	[ "$os" = windows ] && ext=".exe"
	GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false \
		-ldflags "-s -w -X main.version=$version" \
		-o "$out/perritite-$os-$arch$ext" .
done
cd "$out"
# One fixed format ("HASH  name"): Git Bash's sha256sum would write
# "HASH *name", and the committed file must match byte for byte.
for f in perritite-*; do
	if command -v sha256sum >/dev/null 2>&1; then
		h=$(sha256sum "$f" | awk '{print $1}')
	else
		h=$(shasum -a 256 "$f" | awk '{print $1}')
	fi
	printf '%s  %s\n' "$h" "$f"
done >SHA256SUMS
