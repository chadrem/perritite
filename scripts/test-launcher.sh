#!/usr/bin/env bash
# Tests the plugin launcher (bin/perritite) against a local fake GitHub
# release: download + checksum verification, cache reuse, refusal of a
# tampered binary, and refusal of a non-local download URL. Runs on macOS,
# Linux and Windows (Git Bash).
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
version=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' .claude-plugin/plugin.json | head -n 1)
py=$(command -v python3 || command -v python)
work=$(mktemp -d)
server=""
cleanup() { [ -n "$server" ] && kill "$server" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

scripts/build-release.sh "$version" "$work/release/v$version"
port=$("$py" -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
"$py" -m http.server "$port" --bind 127.0.0.1 --directory "$work/release" >/dev/null 2>&1 &
server=$!
for _ in $(seq 50); do curl -fs "http://127.0.0.1:$port/" >/dev/null 2>&1 && break; sleep 0.2; done

export PERRITITE_RELEASE_URL="http://127.0.0.1:$port"
export XDG_CACHE_HOME="$work/cache"

out=$(bin/perritite version) || fail "first run failed"
[ "$out" = "perritite $version" ] || fail "first run printed '$out'"
echo "ok: downloads, verifies and runs the release binary"

kill "$server"; server=""
out=$(bin/perritite version) || fail "cached run failed"
[ "$out" = "perritite $version" ] || fail "cached run printed '$out'"
echo "ok: reuses the cached binary without the network"

"$py" -m http.server "$port" --bind 127.0.0.1 --directory "$work/release" >/dev/null 2>&1 &
server=$!
for _ in $(seq 50); do curl -fs "http://127.0.0.1:$port/" >/dev/null 2>&1 && break; sleep 0.2; done
sed -i.orig 's/^[0-9a-f]\{8\}/00000000/' "$work/release/v$version/SHA256SUMS"
export XDG_CACHE_HOME="$work/cache2"
if err=$(bin/perritite version 2>&1); then fail "ran a binary whose checksum did not match"; fi
case "$err" in *"checksum mismatch"*) ;; *) fail "unexpected error: $err" ;; esac
ls "$work/cache2/perritite/bin/$version/" 2>/dev/null | grep -q '^perritite-' && fail "left a tampered binary in the cache"
echo "ok: refuses a binary whose checksum does not match"

export PERRITITE_RELEASE_URL="https://evil.example"
if err=$(bin/perritite version 2>&1); then fail "accepted a non-local release URL"; fi
case "$err" in *"must be http://127.0.0.1"*) ;; *) fail "unexpected error: $err" ;; esac
echo "ok: refuses a non-local download URL"
