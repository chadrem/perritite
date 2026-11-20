#!/usr/bin/env bash
# Tests the plugin launcher (bin/perritite) against a local fake GitHub
# release, using a throwaway copy of the plugin with its own committed-style
# launcher/SHA256SUMS. Runs on macOS, Linux and Windows (Git Bash).
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
py=$(command -v python3 || command -v python)
work=$(mktemp -d)
server=""
cleanup() { [ -n "$server" ] && kill "$server" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
version=0.0.1

plugin="$work/plugin"
mkdir -p "$plugin/.claude-plugin" "$plugin/launcher"
cp -R "$root/bin" "$plugin/bin"
printf '{\n  "name": "perritite",\n  "version": "%s"\n}\n' "$version" >"$plugin/.claude-plugin/plugin.json"
"$root/scripts/build-release.sh" "$version" "$work/release/v$version"
cp "$work/release/v$version/SHA256SUMS" "$plugin/launcher/SHA256SUMS"
launcher="$plugin/bin/perritite"

port=$("$py" -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
serve() {
	"$py" -m http.server "$port" --bind 127.0.0.1 --directory "$work/release" >/dev/null 2>&1 &
	server=$!
	for _ in $(seq 50); do curl -fs "http://127.0.0.1:$port/" >/dev/null 2>&1 && return; sleep 0.2; done
	fail "fake release server did not start"
}
serve
export PERRITITE_RELEASE_URL="http://127.0.0.1:$port"
export XDG_CACHE_HOME="$work/cache"
cached=$(find "$work" -maxdepth 0)/cache/perritite/bin/$version

out=$("$launcher" version) || fail "first run failed"
[ "$out" = "perritite $version" ] || fail "first run printed '$out'"
echo "ok: downloads, verifies and runs the release binary"

kill "$server"; server=""
out=$("$launcher" version) || fail "cached run failed"
[ "$out" = "perritite $version" ] || fail "cached run printed '$out'"
echo "ok: reuses the verified cached binary without the network"

serve
bin=$(ls "$cached"/perritite-* | head -n 1)
printf '#!/bin/sh\necho PWNED\n' >"$bin"
out=$("$launcher" version) || fail "run after cache tampering failed"
[ "$out" = "perritite $version" ] || fail "ran a tampered cached binary: '$out'"
echo "ok: a tampered cached binary is replaced, never run"

for f in "$work/release/v$version"/perritite-*; do printf 'garbage' >"$f"; done
rm -rf "$work/cache"
if err=$("$launcher" version 2>&1); then fail "ran a downloaded binary that does not match the committed checksum"; fi
case "$err" in *"checksum mismatch"*) ;; *) fail "unexpected error: $err" ;; esac
[ -z "$(ls "$cached" 2>/dev/null | grep '^perritite-' || true)" ] || fail "left a tampered binary in the cache"
echo "ok: refuses a download that does not match the committed checksum"

for bad in "https://evil.example" "http://localhost:1@127.0.0.1:$port" "http://127.0.0.1:${port}@evil.example" "http://localhost:1.evil.com"; do
	export PERRITITE_RELEASE_URL="$bad"
	if err=$("$launcher" version 2>&1); then fail "accepted release URL $bad"; fi
	case "$err" in *"must be exactly http://127.0.0.1"*) ;; *) fail "unexpected error for $bad: $err" ;; esac
done
echo "ok: refuses non-local and userinfo-smuggled download URLs"
