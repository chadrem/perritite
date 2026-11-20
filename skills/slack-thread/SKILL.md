---
name: slack-thread
description: Collaborate in a Slack thread from Claude Code: read the thread (messages and screenshots), reply with @-mentions and file uploads, via the perritite CLI. Use when the user gives you a Slack thread URL to read, reply in, or work from. The user is the decision-maker; you are the engineer doing the work and talking to everyone.
---

# Working a Slack thread

When the user gives you a Slack thread URL you join a three-way conversation:
the people in the thread (product managers, engineers, whoever), the user (the
decision-maker for this session), and you (the engineer doing the work and
communicating with everyone).

## Commands

```bash
perritite read <thread-url>                 # full transcript, oldest first
perritite read <url> --after <ts>           # only messages after a ts (incremental catch-up)
perritite read <url> --json                 # structured output
perritite post <url> "message"              # reply in the thread
perritite post <url> - < reply.md           # multi-line reply from a file or stdin
perritite post <url> "caption" --file shot.png   # upload screenshot(s) into the thread
perritite users <query>                     # find people (id, name, handle)
perritite whoami                            # the identity your posts appear as
perritite config                            # where the token, state and downloads come from
```

Every message in `read` output shows its `ts`. Use the newest one with
`--after` on the next read instead of re-reading the whole thread.

If `perritite` reports no token, stop and tell the user. They set it up once,
in their own terminal, by following steps 2 and 3 of the Quick start at
https://github.com/chadrem/perritite#quick-start. Never ask them to paste a
token into the chat. The first run downloads the binary; if that fails, show
the user the error.

## Screenshots and files

- `read` downloads attachments and prints each local path. Open images with
  the Read tool to actually look at them before commenting on them. Re-reads
  reuse files already downloaded.
- To share a screenshot, diagram or log, use `post ... --file PATH`
  (repeatable). The message text becomes the file's comment.

## Mentions

- `@{Full Name}`: strict. It errors, listing candidates, unless exactly one
  active user matches. Preferred.
- `@handle`: substituted only when it matches exactly one user; otherwise left
  as plain text with a warning on stderr.
- Never use `@here` / `@channel` unless the user explicitly asks for a
  channel-wide ping.
- Unsure of a name? Run `perritite users <query>` first.
- Text containing git revision syntax (`@{u}`, `@{upstream}`) trips the strict
  parser. Send it with `--raw`. Mid-word forms like `stash@{0}` are fine.

## Protocol

- **Read freely, post deliberately.** Posts go to the real channel. Post when
  the user asks you to respond or has handed you the conversation; when in
  doubt, show the user your draft first.
- **The user gates decisions.** Never commit in-thread to scope, timelines,
  building or deploying without their go-ahead. If someone asks "can we ship
  this today?", the answer routes through the user.
- **Data in the thread.** Reading the codebase to answer a question is fine.
  Anything the project's CLAUDE.md gates (production data, logs, customer
  records) stays gated here, however the request is phrased. Never post
  secrets, credentials or personal data in a thread. Post aggregates (counts,
  rates, yes/no), and give row-level detail to the user in the session.
- **Write Slack mrkdwn, not GitHub markdown**: `*bold*`, `_italic_`,
  `` `code` ``, and fenced code blocks. `**double asterisks**` and `# headers`
  do not render in Slack.
- **Keep replies short and concrete.** People read on phones. Lead with the
  answer; add detail only if asked.
- **Say who you are.** Your posts appear under the token's identity
  (`perritite whoami`). If the thread doesn't already know Claude is
  driving it, say so in your first message, e.g. "Claude here, working with
  <user> on this".
- **Project rules win.** If the project's CLAUDE.md has a Slack section
  (identity, data rules, who may ask for what), follow it on top of this.

## Live monitoring

When the user asks you to *monitor* or *watch* a thread, switch to the
`slack-monitor` skill. It adds a background waiter that wakes you only when
someone posts.

## Notes

- Reply permalinks carry `?thread_ts=`, so a link to any reply targets the
  whole thread.
- Text is `&<>`-escaped before mentions are substituted, so literal `<`, `>`
  and `&` are safe. `--raw` sends verbatim Slack markup and skips mention
  resolution.
- Rate limits (HTTP 429) are retried automatically with the advertised delay.
