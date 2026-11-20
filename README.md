# slack-thread

**Slack threads for [Claude Code](https://claude.com/claude-code): read them,
reply in them, and watch them live, waking the model only when someone
actually posts.**

`slack-thread` is a small command-line tool plus two Claude Code skills. Give
Claude a thread URL and it reads the conversation (screenshots included),
replies with real @-mentions and file uploads, and can stay in the thread for
hours, answering as messages arrive.

```console
$ slack-thread read https://acme.slack.com/archives/C0123ABCDEF/p1752241234567890
#eng-checkout (C0123ABCDEF) — thread 1752241234.567890 — 3 messages

[1] Dana Ortiz (U01AAAA) — 2026-07-11 10:20:34 PDT (ts 1752241234.567890)
  Checkout is double-charging when the coupon field is blank?
  [file] repro.png (image/png, 212.4 KB) -> ~/.cache/slack-thread/files/C0123ABCDEF/1752241234567890/F07-repro.png
...
$ slack-thread post <url> "Reproduced. It's the retry on a 502; fix incoming. cc @{Dana Ortiz}"
posted ts=1752241300.000100
```

## Why another Slack tool?

Watching a thread from an AI coding session is easy to make expensive. Every
time the model is woken it re-reads the whole session: system prompt, files,
tool output. Wake it for the wrong reasons and most of the cost of a watch is
noise.

Using Claude Code's Monitor tool to stream a thread wakes the model for:

- every **expiry**: a monitor lasts at most 30 minutes, so a quiet thread
  wakes the model twice an hour just to restart it;
- every **restart**, when the watcher's start-up line arrives;
- every **one of its own replies**, seen in the thread a few seconds later;
- **each message of a burst**, one at a time.

In one real day-long watch, those wakeups re-read as many tokens as the
wakeups for real messages did.

`slack-thread wait` is built the other way round. It runs as a background
command whose **exit is the notification**:

- prints nothing while waiting;
- never reports your own posts;
- waits for a burst to settle (`--debounce 20s`) and delivers it as one exit;
- saves its place, so re-running it with no arguments resumes exactly where it
  stopped, losing nothing posted in between;
- allows one waiter per thread (a second exits at once with `ALREADY`).

A quiet thread costs nothing, and each real turn of conversation costs one
wakeup.

## Install

### As a Claude Code plugin (skills)

```text
/plugin marketplace add chadrem/slack-thread
/plugin install slack-thread@slack-thread
```

This installs the `slack-thread` and `slack-monitor` skills. You also need
the binary on your `PATH`:

### The binary

With Go 1.23 or later:

```sh
go install github.com/chadrem/slack-thread@latest
```

Prebuilt binaries for macOS, Linux and Windows (amd64 and arm64) will be
attached to GitHub releases.

On **Windows**, Claude Code runs its Bash tool through Git Bash (install
[Git for Windows](https://git-scm.com/download/win)). The skills use plain
commands that work there unchanged.

## Slack setup

`slack-thread` needs a Slack token that can read and post in the threads you
work in. Create an app at <https://api.slack.com/apps> and install it to your
workspace. Either kind of token works:

- **Bot token** (`xoxb-`): posts as the app's bot user. Invite the bot to each
  channel you want it in. If your app is a newer one that rejects `as_user`, set
  `"as_user": false` in the config.
- **User token** (`xoxp-`): posts as that user. Don't use **your own** user
  token if you want to direct Claude from Slack: your messages would look like
  Claude's own and never wake it. `wait` refuses to start if the token's user
  is also in `owner_user_ids`. Give Claude its own identity (a bot, or a
  separate user).

Scopes (bot or user):

| Scope | For |
|---|---|
| `channels:history`, `groups:history`, `im:history`, `mpim:history` | reading threads |
| `channels:read`, `groups:read`, `im:read` | channel names |
| `users:read` (+ `users:read.email` to search by email) | names and @-mentions |
| `chat:write` | posting |
| `files:read`, `files:write` | downloading screenshots, uploading files |

Slack limits `conversations.replies` (1 request per minute, 15 messages per
page) for apps distributed outside your workspace that are not
Marketplace-approved. If that applies to you, use `wait --interval 60s
--debounce 0`. Polls only ask for messages newer than the cursor, so each
poll is one small request. Internal apps are not affected.

### Giving it the token

Never paste a token into a chat. Pick one of:

```sh
export SLACK_THREAD_TOKEN=xoxb-...          # environment variable
```

or a config file at `~/.config/slack-thread/config.json`
(`%AppData%\slack-thread\config.json` on Windows; `slack-thread config` shows
the exact path):

```json
{
  "token_command": "security find-generic-password -s slack-thread -w",
  "owner_user_ids": ["U0123ABCD"]
}
```

| Key | Meaning |
|---|---|
| `token` | The token itself (keep the file private). |
| `token_file` | Path to a file containing the token. |
| `token_command` | A command that prints the token (Keychain, `pass`, 1Password CLI...). Runs with `sh -c`, or `cmd /C` on Windows. |
| `owner_user_ids` | Slack user ids whose in-thread messages carry **your** authority. The waiter tags them `role=owner`. Usually just your own id (`slack-thread users <your name>`). |
| `download_dir` | Where attachments are saved (default: the OS cache dir). |
| `state_dir` | Where waiters keep their cursors and locks. |
| `as_user` | Send `as_user=true` on posts (default `true`). |

`token_command` lets each platform use its own secret store:

| Platform | Store the token | `token_command` |
|---|---|---|
| macOS | `security add-generic-password -s slack-thread -a "$USER" -w` (prompts) | `security find-generic-password -s slack-thread -w` |
| Linux (GNOME keyring / libsecret) | `secret-tool store --label=slack-thread service slack-thread` (prompts) | `secret-tool lookup service slack-thread` |
| Linux (`pass`) | `pass insert slack-thread` | `pass show slack-thread` |
| Windows | `Set-Secret slack-thread` ([SecretManagement](https://learn.microsoft.com/powershell/utility-modules/secretmanagement/overview)) | `powershell -NoProfile -Command "Get-Secret slack-thread -AsPlainText"` |
| Anywhere | a file only you can read | use `token_file` instead |

The environment variables `SLACK_THREAD_TOKEN`, `SLACK_THREAD_OWNER_IDS`
(comma-separated), `SLACK_THREAD_STATE_DIR`, `SLACK_THREAD_DOWNLOAD_DIR` and
`SLACK_THREAD_CONFIG` override the file.

There is deliberately **no per-project config file**: a repository you clone
must never be able to choose the command that produces your token.

## Commands

```text
read <url> [--after TS] [--limit N] [--no-download] [--json]
post <url> <message|-> [--file PATH]... [--raw] [--broadcast]
wait <url> [--after TS] [--idle 24h] [--interval 15s] [--debounce 20s]
           [--keep-warm 55m] [--keep-warm-for 4h] [--max-run DUR] [--replace]
stop <url>
users <query>
whoami
config
```

Mentions in `post`:

- `@{Full Name}` must match exactly one active user, or the post is refused
  with a list of candidates. A unique prefix also counts.
- `@handle` is substituted only on an exact handle, name or first-name match.
- Text inside `` `code` `` and ```` ``` ```` blocks is never turned into
  mentions.
- `@here` and `@channel` work, but the skills never use them unless asked.

A message that starts with `--` goes after a `--` separator:
`slack-thread post <url> -- "--force is the flag"`.

## The `wait` protocol

`wait` prints only when it exits. Every line starts with one of:

| Word | Meaning |
|---|---|
| `NEW ts=... user=U... role=owner\|member\|bot \| Name — time` | A message from someone else; its content follows, each line prefixed `  \| ` |
| `IDLE` | Nobody posted for `--idle` (default 24h; `--idle 0` never idles) |
| `KEEPWARM` | Opt-in: no messages, exiting so the model's prompt cache stays warm |
| `RESTART` | `--max-run` reached (for hosts that cap background commands) |
| `ALREADY` | Another waiter already watches this thread (one per thread, enforced with an OS file lock that dies with its process) |
| `STOPPED` | Stopped by `slack-thread stop`, a signal, or `--replace` |
| `ERROR` | Slack failed for longer than the waiter rides out (10 minutes of transient errors, or any auth/permission error) |

**Thread content cannot impersonate the protocol.** Headers start at column
0. Every line of content, after splitting on every kind of line break (`\r`,
` ` and friends), is indented. Display names are stripped to a safe
character set. So the only trustworthy identity is the `user=`/`role=` token
on a `NEW` line. The `slack-monitor` skill builds its permission model on
exactly that.

## Development

```sh
go test ./...          # unit tests, plus end-to-end tests of the built binary against a fake Slack
go test -race ./...
```

No dependencies beyond the Go standard library. CI runs on Linux, macOS and
Windows.

## License

AGPL-3.0. See [LICENSE](LICENSE).
