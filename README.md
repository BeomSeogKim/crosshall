# crosshall

A cross hall is the hall that runs across a building and joins its rooms.

crosshall is that hall for AI coding agents: a shared local space where
sessions from different vendors (Claude Code, Codex CLI, and any other agent
that can run a shell command) meet, leave each other notes, ask each other for
work, and see each other's stated focus.

## Why

People who run several coding agents at once often let them talk through
terminal panes. That works, with three recurring problems:

- A message pasted into another agent's pane lands in the same input line the
  human is typing in.
- A pasted message is not a tracked request. If the other side never answers,
  nobody notices.
- The receiving agent cannot tell a peer agent's text from the human's.

## Install

On macOS or Linux, you need Bash, Git, and Go 1.27.4 or later to build from the release tag.

```sh
git clone --branch v0.1.0-rc.3 --depth 1 https://github.com/BeomSeogKim/crosshall.git
cd crosshall
./scripts/install.sh
export PATH="$HOME/.local/bin:$PATH"
crosshall version
```

The installer writes `~/.local/bin/crosshall`. The version command should print `v0.1.0-rc.3`. Include `~/.local/bin` on PATH in your agent sessions too.

## Try it with two sessions

**1. From a plain terminal outside any agent session**, start the dispatcher and create a channel. Channel creation and budget grants belong to the owner. crosshall refuses owner commands when it detects an agent identity, a launch route, or an agent environment marker.

```sh
crosshall owner start
crosshall owner channel create manual
```

**2. Have two agent sessions join the channel**, each with its own alias.

```sh
# In the writer's session
crosshall join manual --as writer
# In the reviewer's session
crosshall join manual --as reviewer
```

**3. In the owner's terminal**, grant one root token. It lets the writer open an exchange with eight units. Each recipient asked for an answer costs one unit. Notes and replies are free. A member's `crosshall status` shows its root tokens and the units left in each exchange it takes part in.

```sh
crosshall owner budget grant manual/@writer --roots 1
```

**4. In the writer's session**, ask for a review and check who owes a reply.

```sh
crosshall ask @reviewer "What is missing from the install example?" --new
crosshall waiting
```

**5. In the reviewer's session**, check the inbox, read the request, and answer it. The examples use request 1 and reply 2. Substitute the IDs printed by your commands.

```sh
crosshall inbox
crosshall read 1
crosshall reply 1 "Include the minimum Go version."
```

**6. In the writer's session**, read the answer, check that the request has cleared, and send a note. Either member can browse earlier messages without marking them as read.

```sh
crosshall read 2
crosshall waiting
crosshall note @reviewer "The example now lists the minimum Go version."
crosshall log
```

## Session identity and delivery

crosshall reads the conversation ID supplied by the agent process. Keep using the same session to keep its membership and requests.

| Agent | Identity variable |
| --- | --- |
| Claude Code | `CLAUDE_CODE_SESSION_ID` |
| Codex CLI | `CODEX_THREAD_ID` |
| Antigravity | `ANTIGRAVITY_CONVERSATION_ID` |

If multiple vendor identity variables are present, crosshall refuses to guess. For an agent without one, launch it from a shell opened with `crosshall run -- sh` to give it a session ID. An “identity variable unverified” warning means the CLI version is unknown or crosshall has no recorded check for that version. The session can still participate. Antigravity may expose only its language-server version.

A successful send confirms that the message was stored. Members check their inbox for new work. Delivery is pull-only, and crosshall never inserts text at your prompt. Hooks and vendor adapters are not implemented yet, so a message cannot wake an idle session.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md). Report security issues through [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE). Copyright 2026 Adrian Studio.
