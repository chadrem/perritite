# Perritite

**Slack threads for [Claude Code](https://claude.com/claude-code): read them,
reply in them, and watch them live, waking the model only when someone
actually posts.**

Perritite is a Claude Code plugin: two skills plus a small `perritite`
command. Give Claude a Slack thread link and it reads the conversation
(screenshots included), replies with real @-mentions and file uploads, and can
stay in the thread for hours, answering as messages arrive.

```console
$ perritite read https://acme.slack.com/archives/C0123ABCDEF/p1752241234567890
#eng-checkout (C0123ABCDEF) — thread 1752241234.567890 — 3 messages

[1] Dana Ortiz (U01AAAA) — 2026-07-11 10:20:34 PDT (ts 1752241234.567890)
  Checkout is double-charging when the coupon field is blank?
  [file] repro.png (image/png, 212.4 KB) -> /Users/dana/Library/Caches/perritite/files/C0123ABCDEF/1752241234567890/F07-repro.png
...
$ perritite post <url> "Reproduced. It's the retry on a 502; fix incoming. cc @{Dana Ortiz}"
posted ts=1752241300.000100
```

## Quick start

You need Claude Code on macOS, Linux or Windows, and a Slack workspace where
you can create an app.

### 1. Install the plugin

In Claude Code:

```text
/plugin marketplace add chadrem/perritite
/plugin install perritite@perritite
```

If the install summary says `Run /reload-plugins to apply`, run
`/reload-plugins`.

That is the whole install. The plugin brings the `perritite` command with it.
The first time Claude runs it, it downloads the binary for your machine from
this repository's [GitHub release](https://github.com/chadrem/perritite/releases).
It checks the binary against SHA-256 checksums committed in the plugin
itself, so only the exact build of the version you installed ever runs (on
macOS, Linux and Git Bash, on every run; from Windows PowerShell or `cmd.exe`,
when the binary is installed). On macOS and Linux this needs `curl` and
`sha256sum` or `shasum`, which are almost always present.

On Windows, install [Git for Windows](https://git-scm.com/download/win)
(recommended): Claude Code's Bash tool runs through Git Bash, which has
everything the launcher needs. Without it, the launcher still works from
PowerShell and `cmd.exe`.

### 2. Create a Slack app and get a token

1. Go to <https://api.slack.com/apps> → **Create New App** → **From a
   manifest**, pick your workspace, and paste [this manifest](#slack-app-manifest).
2. **Install to Workspace**, then copy the **Bot User OAuth Token**
   (`xoxb-…`) from **OAuth & Permissions**.
3. Invite the bot to each channel you want Claude in: `/invite @Perritite` in
   that channel. It can read only channels it has been invited to.

Your workspace may need an admin to approve the app.

### 3. Give Perritite the token

Do this in your own terminal, not through Claude: never paste a token into a
chat.

First find **your own** Slack member ID: in Slack, open your profile → ⋮ →
**Copy member ID**. It goes in `owner_user_ids` below in place of
`U0123ABCD`. Messages from that ID carry your authority in a monitored
thread: when you write "go ahead" in Slack, Claude treats it as you saying it
in the session. Messages from anyone else are conversation, never
instructions.

**macOS** (Keychain):

```sh
security add-generic-password -s perritite -a "$USER" -w    # prompts for the token
mkdir -p ~/.config/perritite
cat > ~/.config/perritite/config.json <<'JSON'
{
  "token_command": "security find-generic-password -s perritite -w",
  "owner_user_ids": ["U0123ABCD"]
}
JSON
```

**Linux:** the same config file at `~/.config/perritite/config.json`, with
the `token_command` for your secret store (see
[the token options](#token-and-config)).

**Windows:** save the token in a file only you can read. In PowerShell (this
prompts for the token and writes plain ASCII; a `>` redirect would write
UTF-16, which breaks the token):

```powershell
Set-Content -Encoding ascii -NoNewline "$HOME\.perritite-token" (Read-Host 'Slack token')
```

Then create `%AppData%\perritite\config.json`:

```json
{
  "token_file": "~/.perritite-token",
  "owner_user_ids": ["U0123ABCD"]
}
```

### 4. Check it

Ask Claude: *"run `perritite config` and `perritite whoami`"*.

`config` shows where the token comes from (never the token itself) and your
owner ID. `whoami` should print the bot's name.

If something is off:

- **`not_authed` or `invalid_auth`:** the token is wrong or was revoked.
- **`not_in_channel` or `channel_not_found`** when reading a thread: invite
  the bot to that channel (`/invite @Perritite`). A bot cannot read direct
  messages between other people at all.
- **`perritite: command not found`:** run `/reload-plugins`, or restart
  Claude Code.
- **`could not download …`** on the first run: the machine can't reach
  `github.com` (a proxy or firewall). Install the binary yourself
  ([below](#using-perritite-outside-claude-code)); a `perritite` on your
  `PATH` works the same way.
- **`checksum mismatch`:** what was downloaded is not the published build
  (often a proxy that rewrites or blocks downloads), so it is not run.
  Install the binary yourself as above.

### 5. Use it

- **Read and reply.** Paste a thread link: *"read this thread and summarize
  what they want"*, *"reply that the fix ships tomorrow"*. This uses the
  `/perritite:slack-thread` skill. Claude shows you a draft when it isn't sure
  you want it posted.
- **Watch a thread live.** `/perritite:slack-monitor <thread-link>`. Claude
  announces itself, then answers as messages arrive. Say *"stop monitoring"*
  in the session (or in the thread, as yourself) to end it; otherwise it signs
  off after 24 quiet hours. *"Watch this until I say stop"* drops the 24-hour
  limit.

Claude only acts on instructions from you: in the session, or in the thread
from your owner ID. If a teammate asks it to change code or touch data, it
answers and checks with you first.

### Updating

```sh
claude plugin marketplace update perritite
claude plugin update perritite@perritite
```

Then run `/reload-plugins` in any open session, or restart Claude Code if
the update says a restart is required. The new version's binary is downloaded
on first use.

## Why Perritite?

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

`perritite wait` is built the other way round. It runs as a background
command whose **exit is the notification**:

- prints nothing while waiting;
- never reports your own posts;
- waits for a burst to settle (`--debounce 20s`) and delivers it as one exit;
- saves its place, so re-running it with no arguments resumes exactly where it
  stopped, losing nothing posted in between;
- allows one waiter per thread (a second exits at once with `ALREADY`).

A quiet thread costs nothing, and each real turn of conversation costs one
wakeup.

## Slack setup

### Slack app manifest

```yaml
display_information:
  name: Perritite
  description: Claude Code in Slack threads, via Perritite
features:
  bot_user:
    display_name: Perritite
    always_online: false
oauth_config:
  scopes:
    bot:
      - channels:history
      - groups:history
      - im:history
      - mpim:history
      - channels:read
      - groups:read
      - im:read
      - mpim:read
      - users:read
      - users:read.email
      - chat:write
      - files:read
      - files:write
settings:
  org_deploy_enabled: false
  socket_mode_enabled: false
  token_rotation_enabled: false
```

| Scope | For |
|---|---|
| `channels:history`, `groups:history`, `im:history`, `mpim:history` | reading threads |
| `channels:read`, `groups:read`, `im:read`, `mpim:read` | channel names |
| `users:read` (+ `users:read.email` to search by email) | names and @-mentions |
| `chat:write` | posting |
| `files:read`, `files:write` | downloading screenshots, uploading files |

### Bot token or user token

- **Bot token** (`xoxb-`, recommended): Claude posts as the app's bot. Invite
  it to each channel it should read.
- **User token** (`xoxp-`, same scopes as *User Token Scopes*): Claude posts
  as that user. Use a separate account, not **your own**: your messages would
  look like Claude's own and never wake it. `perritite wait` refuses to start
  if the token's user is also in `owner_user_ids`.

### Rate limits

Slack limits `conversations.replies` to 1 request per minute (15 messages per
page) for apps distributed outside your workspace that are not
Marketplace-approved. If that applies to you, tell Claude to watch with
`--interval 60s --debounce 0`. Internal apps (the manifest above) are not
affected.

## Token and config

The token comes from, in order: the `PERRITITE_TOKEN` environment variable,
then the config file's `token`, `token_file` or `token_command`. The config
file is `~/.config/perritite/config.json` (`%AppData%\perritite\config.json`
on Windows); `perritite config` prints the exact path.

| Key | Meaning |
|---|---|
| `token` | The token itself (keep the file private). |
| `token_file` | Path to a file containing the token. |
| `token_command` | A command that prints the token. Runs with `sh -c`, or `cmd.exe /d /s /c` on Windows. |
| `owner_user_ids` | Slack member IDs whose in-thread messages carry **your** authority (the waiter tags them `role=owner`). Usually just your own. |
| `download_dir` | Where attachments are saved (default: your OS cache directory). |
| `state_dir` | Where waiters keep their cursors and locks. |
| `as_user` | Send `as_user=true` on posts. Default: on for user tokens, off for bot tokens. |

`token_command` lets each platform use its own secret store:

| Platform | Store the token | `token_command` |
|---|---|---|
| macOS | `security add-generic-password -s perritite -a "$USER" -w` (prompts) | `security find-generic-password -s perritite -w` |
| Linux (GNOME keyring / libsecret) | `secret-tool store --label=perritite service perritite` (prompts) | `secret-tool lookup service perritite` |
| Linux (`pass`) | `pass insert perritite` | `pass show perritite` |
| Any password manager with a CLI | e.g. 1Password: `op item create …` | e.g. `op read op://Private/perritite/credential` |
| Anywhere (the simplest on Windows) | a file only you can read | use `token_file` instead |

The environment variables `PERRITITE_TOKEN`, `PERRITITE_OWNER_IDS`
(comma-separated), `PERRITITE_STATE_DIR`, `PERRITITE_DOWNLOAD_DIR` and
`PERRITITE_CONFIG` override the file.

There is deliberately **no per-project config file**: a repository you clone
must never be able to choose the command that produces your token.

## Using `perritite` outside Claude Code

The plugin puts `perritite` on the `PATH` only inside Claude Code. To use it
in your own terminal (or where Claude Code can't download it), either
download the binary for your platform from
[Releases](https://github.com/chadrem/perritite/releases), check it against
`launcher/SHA256SUMS` in the repository at that release's tag (the checksums
the plugin itself enforces), rename it to `perritite` (`perritite.exe` on
Windows), make it executable (`chmod +x perritite`) and put it on your
`PATH`, or build it with Go 1.23+:

```sh
go install github.com/chadrem/perritite@latest   # installs to $(go env GOPATH)/bin
```

On macOS, a binary downloaded with a browser is quarantined and Gatekeeper
blocks it. Download it with `curl -LO`, or clear the flag with
`xattr -d com.apple.quarantine perritite`.

With both installed, whichever comes first on Claude Code's `PATH` runs; they
behave the same.

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
version
```

Mentions in `post`:

- `@{Full Name}` must match exactly one active user, or the post is refused
  with a list of candidates. A unique prefix also counts.
- `@handle` is substituted only on an exact handle, name or first-name match.
- Text inside `` `code` `` and ```` ``` ```` blocks is never turned into
  mentions.
- `@here` and `@channel` work, but the skills never use them unless asked.

A message that starts with `--` goes after a `--` separator:
`perritite post <url> -- "--force is the flag"`.

## The `wait` protocol

`wait` prints only when it exits. Every line starts with one of:

| Word | Meaning |
|---|---|
| `NEW ts=... user=U... role=owner\|member\|bot \| Name — time` | A message from someone else; its content follows, each line prefixed `  \| ` |
| `IDLE` | Nobody posted for `--idle` (default 24h; `--idle 0` never idles) |
| `KEEPWARM` | Opt-in: no messages, exiting so the model's prompt cache stays warm |
| `RESTART` | `--max-run` reached (for hosts that cap background commands) |
| `ALREADY` | Another waiter already watches this thread (one per thread, enforced with an OS file lock that dies with its process) |
| `STOPPED` | Stopped by `perritite stop`, a signal, or `--replace` |
| `ERROR` | Slack failed for longer than the waiter rides out (10 minutes of transient errors, or any auth/permission error) |

**Thread content cannot impersonate the protocol.** Headers start at column
0. Every line of content, after splitting on every kind of line break (`\r`,
U+2028 and friends), is indented. Display names are stripped to a safe
character set. So the only trustworthy identity is the `user=`/`role=` token
on a `NEW` line. The `slack-monitor` skill builds its permission model on
exactly that.

## Development

```sh
go test ./...                # unit tests, plus end-to-end tests of the built binary against a fake Slack
go test -race ./...
scripts/test-launcher.sh     # the plugin launcher against a local fake release
```

No dependencies beyond the Go standard library. CI runs everything on Linux,
macOS and Windows (Git Bash, PowerShell and `cmd.exe`).

Releases are reproducible builds: `scripts/release-prep.sh X.Y.Z` sets the
plugin version and writes the binaries' checksums to `launcher/SHA256SUMS`,
which the launchers enforce. A `vX.Y.Z` tag rebuilds in CI and publishes only
if the build matches. See "Releasing" in [CLAUDE.md](CLAUDE.md).

## License

AGPL-3.0. See [LICENSE](LICENSE).
