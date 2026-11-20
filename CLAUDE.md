# Perritite

A Go CLI plus two Claude Code skills (`skills/slack-thread`,
`skills/slack-monitor`) for working in Slack threads from a Claude Code
session. The repo is also a Claude Code plugin marketplace
(`.claude-plugin/`). Public, AGPL-3.0, standard library only.

## Layout

- `main.go`: entry point; `version` is stamped by the release build.
- `internal/slack`: minimal Slack Web API client (form POSTs, 429 retries,
  cursor pagination, external file upload, token-safe downloads).
- `internal/thread`: URL parsing, timestamp ordering, mention resolution,
  read/post, attachment downloads.
- `internal/watch`: `wait`, the background waiter, with its state files and
  per-thread lock.
- `internal/frame`: output framing (see invariants).
- `internal/config`: config file plus env overrides, token resolution.
- `internal/cli`: argv parsing and rendering.
- `internal/slacktest`: fake Slack over httptest, used by every test.
- `e2e_test.go`: builds the binary and runs it as real processes.
- `bin/perritite` (POSIX sh) and `bin/perritite.cmd` plus
  `launcher/perritite-fetch.ps1` (Windows): the plugin launchers. Claude Code
  puts the plugin's `bin/` on PATH. They download the release binary for
  `.claude-plugin/plugin.json`'s `version` and verify it against
  `launcher/SHA256SUMS`, which is committed:
  - the sh launcher caches the binary and re-verifies it on every run;
  - the Windows one installs a verified `bin/perritite.exe`, which Windows
    then finds ahead of the `.cmd`, so arguments skip cmd.exe parsing.

  `scripts/test-launcher.{sh,ps1}` test them against a local fake release.
- `scripts/build-release.sh`: reproducible release binaries plus
  `SHA256SUMS` (exact toolchain from go.mod's `toolchain` line, `-trimpath`,
  `-buildvcs=false`). `scripts/release-prep.sh X.Y.Z` sets the version and
  writes `launcher/SHA256SUMS`. CI's `reproducible` job proves Linux, macOS
  and Windows builds are byte-identical.

## Commands

```sh
go test ./...            # everything, including e2e
go test -race ./...
go vet ./...
gofmt -l .               # must print nothing
GOOS=windows go vet ./...   # Windows-only files compile separately
```

## Invariants: do not break these

1. **`wait` never prints while waiting, and exits on every outcome.** Its exit
   is the model's notification; any stray output line is wasted, and in a
   Monitor-style consumer it would be a wakeup.
2. **Own posts never wake anyone.** Messages whose `user` equals the token's
   `auth.test` user id advance the cursor silently.
3. **The saved cursor never passes an unprinted message.** `wait` tracks
   `seen` (how far it has read) separately from `State.Cursor` (committed).
   The cursor moves past an own post only when nothing is pending before it,
   and past pending messages only after printing them. A stop, kill or
   crash while a burst settles therefore re-delivers instead of losing.
   `TestAStopWhileSettlingRedeliversThePendingMessage` guards it.
4. **The framing is a security boundary.** Protocol lines (`NEW`, `IDLE`, ...)
   start at column 0. Every line of thread-authored content goes through
   `frame.Quote`/`frame.Indent`, which split on every Unicode line break, not
   just `\n`. Display names go through `frame.SafeAuthor`. The skills
   authorize actions by the `user=`/`role=` tokens alone, so a regression here
   is a privilege escalation. `TestThreadContentCanNeverForgeAProtocolLine`
   and `TestReadContentCannotForgeAColumnZeroHeader` guard it.
5. **The token goes only to Slack hosts.** Downloads follow redirects by hand
   (`slack.Client.Download`) so a signed CDN URL never sees it. Uploads to
   pre-signed URLs send no token.
6. **No per-project config.** A cloned repo must not be able to set
   `token_command`. Config is user-level and env only.
7. **Idle is measured across runs** (`State.LastActivity`), so re-arming
   after each message doesn't reset the 24-hour clock.
8. **The lock is an OS file lock** (`flock`; on Windows an exclusive-write
   handle), held for the waiter's whole life and released by the OS however
   the process dies. Never delete a lock file: deleting a locked file lets a
   second waiter lock a new one beside it. `stop` and `--replace` signal the
   pid recorded by a lock that is *currently held*, so it is never a
   recycled pid.
9. **Never poll faster than `--interval`**, even while settling a burst.
   Rate-limited apps depend on it.
10. **Standard library only.** Cross-platform single static binaries are the
   point.

11. **Launchers run only binaries matching the committed
    `launcher/SHA256SUMS`.** Never trust the release's own checksum file:
    whoever can replace an asset can replace that too.
    `PERRITITE_RELEASE_URL` (the test hook) is honoured only as exactly
    `http://127.0.0.1:<port>` or `http://localhost:<port>`, and the
    committed checksum is enforced even then.
12. **`plugin.json` `version`, the tag, and `launcher/SHA256SUMS` move
    together.** The release workflow refuses a tag that doesn't match the
    version, and refuses to publish a build that doesn't match the committed
    checksums. Set `version` only in `plugin.json`, never also in
    `marketplace.json`.
13. **Any Go change after `release-prep.sh` invalidates the checksums.** Run
    it last, after the final code change for the release.

## Releasing

Plugin installs track `main`, so `main` must never carry a version whose
release isn't published yet:

1. On a branch, finish all code changes, then run
   `scripts/release-prep.sh X.Y.Z` and commit `plugin.json` plus
   `launcher/SHA256SUMS` ("Release vX.Y.Z").
2. `git tag vX.Y.Z && git push origin vX.Y.Z`. Push only the tag.
3. Wait for the release workflow to publish it.
4. Fast-forward `main` to the tag and push `main`.

Users update with `claude plugin marketplace update perritite` and then
`claude plugin update perritite@perritite`.

## Cross-platform notes

- Locking is build-tagged (`internal/watch/lock_{unix,windows}.go`).
  Windows has no SIGTERM, so a stop there is a hard kill. That is safe because
  state is always written atomically (temp file + rename).
- `token_command` runs under `sh -c`. On Windows it runs under `cmd.exe /s /c`
  with a verbatim command line (`internal/config/shell_windows.go`), because
  Go's default argument quoting is not what cmd.exe parses.

## Testing conventions

- Tests talk real HTTP to `slacktest.Server`; do not add interface mocks of
  the Slack client.
- Waiter tests use `fakeClock`, whose `Sleep` advances time and runs an
  `onSleep` hook that changes the fake thread between polls. A 24-hour watch
  runs in milliseconds; keep long-horizon tests at a large `Interval` so they
  stay fast.
- When you add behaviour, check the test can fail: break the code on purpose
  and confirm a test goes red.

## Commits

Subject about 50 characters, imperative mood, no trailing period. Add a body
only for *why*. No AI attribution lines (`.claude/settings.json` disables
them).
