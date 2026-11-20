#!/bin/sh
# Prepares a release: scripts/release-prep.sh X.Y.Z
# Sets the plugin version and writes launcher/SHA256SUMS from a reproducible
# build. Commit both, then follow "Releasing" in CLAUDE.md.
set -eu
version=$1
case "$version" in *[!0-9.]* | "") echo "version must look like 1.2.3" >&2; exit 1 ;; esac
cd "$(dirname "$0")/.."
sed -i.bak "s/\"version\": \"[^\"]*\"/\"version\": \"$version\"/" .claude-plugin/plugin.json && rm -f .claude-plugin/plugin.json.bak
grep -q "\"version\": \"$version\"" .claude-plugin/plugin.json || { echo "could not set the version in plugin.json" >&2; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
scripts/build-release.sh "$version" "$tmp"
cp "$tmp/SHA256SUMS" launcher/SHA256SUMS
echo "plugin.json is at $version and launcher/SHA256SUMS is written:"
cat launcher/SHA256SUMS
