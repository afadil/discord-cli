# discord-cli

A small command-line tool for reading and posting Discord messages as a bot.

It only uses Discord's REST API and never opens a gateway connection, so it
does not count toward the bot's 1000 logins (IDENTIFY) per day. Running it
from many agent sessions at once costs nothing. It has no dependencies outside
Go's standard library.

Why: gateway-based Discord MCP servers log the bot in once per agent session.
With a few coding agents open, those logins add up, and when a bot passes 1000
in a day Discord resets its token. A CLI that only makes REST calls avoids that
entirely, and agents can use it through their shell.

Built for our own use. Issues and pull requests are welcome, but there is no
promise of a response or a fix.

## Requirements

- Go 1.22 or later.
- macOS for Keychain token storage. Elsewhere, set `DISCORD_TOKEN` instead.
- A Discord bot added to your server, with:
  - **Message Content Intent** enabled (Developer Portal → your app → Bot).
    Without it, message text and attachments come back empty.
  - Permissions **View Channels** and **Read Message History** for reading,
    plus **Send Messages** and **Send Messages in Threads** for `send`.

## Install

```bash
go install github.com/afadil/discord-cli/cmd/discord@latest
```

This puts `discord` in `$(go env GOPATH)/bin`, which must be on your `PATH`.
To build from a checkout instead:

```bash
git clone https://github.com/afadil/discord-cli.git
cd discord-cli
go build -trimpath -ldflags='-s -w' -o ~/.local/bin/discord ./cmd/discord
```

## Bot token

Store the token in the Keychain. The command prompts for the value, so it stays
out of your shell history:

```bash
security add-generic-password -U -s discord-cli -a bot-token -w
```

Run the same command after resetting the token. `DISCORD_TOKEN` is used only
when the Keychain has no entry.

## Use

```text
discord servers
discord channels [--server ID]
discord posts FORUM_ID [--limit N]
discord read CHANNEL_ID [--limit N] [--before MSG_ID | --after MSG_ID] [--download]
discord send CHANNEL_ID [--reply-to MSG_ID] [TEXT]
```

Examples:

```bash
# Find channel and thread IDs. --server is only needed if the bot is in several servers.
discord channels

# Last 20 messages of a channel, oldest first.
discord read CHANNEL_ID --limit 20

# A thread or forum post, with attachments saved locally; each file's path is printed.
discord read THREAD_ID --download

# Older history: page back from the oldest message you have.
discord read CHANNEL_ID --before MSG_ID

# Forum posts: all open ones, then the 25 most recently archived (--limit N).
discord posts FORUM_ID

# Reply in a thread. Text comes from stdin when TEXT is omitted.
discord send THREAD_ID --reply-to MSG_ID <<'EOF'
Thanks for the report, this is fixed in the next release.
EOF
```

You can also copy IDs from the Discord app: turn on Developer Mode (Settings →
Advanced), then right-click a channel, thread or message → Copy ID.

Behavior worth knowing:

- `--json` prints the raw Discord response instead of text. It can't be
  combined with `--download`.
- Flags may come before or after IDs. Everything after `--` is text, so
  `discord send ID -- -5% today` works; stdin is simpler for longer messages.
- `read --download` saves attachments up to 25 MB into a new temp folder.
  Downloads only go to Discord's CDN hosts and never carry the token.
- Forwarded messages print their forwarded text and attachments.
- Reads wait out short rate limits (up to 10 s).
- `send` disables all mentions, so it never pings anyone, and it never retries.
  Discord caps a message at 2000 characters.

## Agent skill

`skill/SKILL.md` tells coding agents how to use the CLI and to ask before every
`send`. Install it for Claude Code and opencode (both read `~/.claude/skills`)
and for Codex:

```bash
mkdir -p ~/.claude/skills/discord ~/.codex/skills/discord
cp skill/SKILL.md ~/.claude/skills/discord/
cp skill/SKILL.md ~/.codex/skills/discord/
```

Copy it again after changing it.

The CLI needs the network and the Keychain, so it can't run inside a command
sandbox. Per agent:

- **Codex** sandboxes commands, which blocks both. Allow the read commands to
  run outside the sandbox by adding these to `~/.codex/rules/default.rules`,
  then restart Codex. `send` has no rule, so Codex asks every time.

  ```text
  prefix_rule(pattern=["discord", "servers"], decision="allow")
  prefix_rule(pattern=["discord", "channels"], decision="allow")
  prefix_rule(pattern=["discord", "posts"], decision="allow")
  prefix_rule(pattern=["discord", "read"], decision="allow")
  ```

- **opencode** runs commands unsandboxed and without asking by default. Make
  `send` ask in `~/.config/opencode/opencode.json`, then restart opencode:

  ```json
  "permission": { "bash": { "*": "allow", "*discord send*": "ask" } }
  ```

- **Claude Code**: to make `send` always ask, add `"Bash(discord send:*)"` to
  `permissions.ask` in `~/.claude/settings.json`.

## Test

```bash
go test ./...
```

Tests run against a local fake Discord server and need no token.

## License

MIT
