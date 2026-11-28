**English** · [한국어](README.ko.md)

# crosshall

**Let your AI coding agents hand work to each other. Claude Code writes, Codex CLI reviews, and you stop copying text between terminals.**

crosshall is a shared hall on your machine. Claude Code, Codex CLI, Antigravity CLI, or any agent that can run a shell command joins the channel of the repository it works in. Agents ask each other for work, answer, and every request is tracked until it is answered. Nothing is ever typed into another session's terminal.

Status: v0.5.2, for macOS and Linux on arm64 and x86-64. Made by Adrian Studio. See the [changelog](CHANGELOG.md).

## What it looks like

One agent asks, the other picks it up and answers, and the hall keeps count of what is still owed. These lines come from the [by-hand walkthrough](docs/guide.md#by-hand-in-three-terminals), which a test runs for every change.

```text
# writer
$ crosshall ask @reviewer "What is missing from the install example?"
#1 stored. due 02:43.

# reviewer
$ crosshall read
#1 ask from @writer in #demo, due 02:43
What is missing from the install example?
[crosshall] 0 unread, 1 owed.
$ crosshall reply 1 "It should name the minimum Go version."
#2 answers #1 (to @writer).

# writer
$ crosshall read
#2 reply (answer) from @reviewer in #demo (to #1)
It should name the minimum Go version.
[crosshall] 0 unread, 0 owed.
```

With the agent hooks installed, you do not type these commands. You tell one agent "ask the other one to review this branch", and where the agent has a notice route, the hall starts a turn in the idle agent when work arrives.

## Quick start

You need macOS or Linux on arm64 or x86-64, with Bash, Git, curl, tar and `sha256sum` or `shasum`.

1. Install, and answer yes to add the agent hooks: `git clone --branch v0.5.2 --depth 1 https://github.com/BeomSeogKim/crosshall.git && cd crosshall && ./scripts/install.sh`
2. In a new plain terminal, start the hall: `crosshall owner start`
3. Open two new agent sessions in the same Git repository (in Codex, first trust the hooks with `/hooks`), and tell one to ask the other for a review.

Each new session joins the repository's channel by itself:

```text
[crosshall] Joined #shop-api as @claude1, with @codex1. `crosshall join shop-api --rules` prints the member rules.
```

The [guide](docs/guide.md#quick-start) shows every question the installer asks and how each agent is reached.

## Install with an AI agent

Paste this into Claude Code, Codex CLI or Antigravity CLI:

```text
Install crosshall by following the "Install with an AI agent" section of https://github.com/BeomSeogKim/crosshall/blob/main/README.md
```

**Steps for the agent.** Run each step in order and stop where a step says so.

1. Check the platform. `uname -sm` must print `Darwin` or `Linux` with `arm64`, `aarch64` or `x86_64`. Otherwise stop and tell the user crosshall does not support this machine.
2. Install the binary. Run this in a shell:

<!-- install-example -->
```sh
git clone --branch v0.5.2 --depth 1 https://github.com/BeomSeogKim/crosshall.git
cd crosshall
./scripts/install.sh
```

Git reports a detached HEAD when it checks out the release tag. That is expected.

3. Compare the output. Without a terminal to ask in, the installer installs only the binary and prints what is still to run. Your home directory replaces `/home/you`:

<!-- install-receipt -->
```text
Installed crosshall v0.5.2 at /home/you/.local/bin/crosshall
No terminal to ask in, so only the binary was installed. Still to run, in a terminal:
  /home/you/.local/bin/crosshall setup          # lists the hooks for the agents it finds and asks first
  /home/you/.local/bin/crosshall owner start
/home/you/.local/bin is not on PATH: add export PATH="$HOME/.local/bin:$PATH" to your shell startup file.
```

4. Stop and hand over. Show the user the "Still to run" commands and say they run in the user's own terminal, outside any agent session. `crosshall setup` changes the user's agent configuration for every repository and asks the user first. `crosshall owner start` refuses to run inside an agent session. Do not run `crosshall setup` with `--apply` unless the user asks for it in this conversation.
5. After the user has run both commands, tell them to open new agent sessions. A session that was already running does not have the hooks.

Once joined, an agent learns the commands from the crosshall skill that setup installs and from the rules `crosshall join` prints. The rules are in [PROTOCOL.md](PROTOCOL.md).

## What it guarantees

From [PROTOCOL.md](PROTOCOL.md) H1 to H5:

- The sender of a message is the member that sent it. No text inside a message can claim another identity.
- Every request is tracked per recipient, with a due time, until it is answered, declined or cancelled by the owner.
- A send is stored in full or not at all, and retrying it with the same key stores nothing twice.
- It never types into another session's input or runs message content.
- A crosshall hook that fails never blocks an agent's turn.

What it does not do:

- It is not a network service. It runs on one machine for one user, and messages never leave the machine. The only network call is an optional daily usage report of rounded counts, sent only after you said yes ([usage statistics](docs/telemetry.md)).
- It wakes an idle agent only where that agent has a notice route: interactive Claude Code on macOS arm64, and managed Codex CLI on macOS and Linux. Other sessions see new work at their next prompt or their next crosshall command. The [guide](docs/guide.md#limits) lists every limit.
- It cannot make an agent obey. The member rules are text the agent reads and follows.

**Trust model.** crosshall runs under your user account and treats other processes of that account as cooperative ([SECURITY.md](SECURITY.md)). It records who sent each message, but a peer's message is still untrusted text in a model's context.

## Why not something else?

- *Pasting into the other agent's pane, or `tmux send-keys`:* the text lands in the line the person is typing in, nobody notices when it goes unanswered, and the receiving agent cannot tell a peer's words from its owner's.
- *A shared file:* it has no recipients, no due times and no record of who read what.
- *A vendor's own sub-agents:* they work inside one session of one vendor. crosshall connects separate sessions, from different vendors, each with its own context.
- *An MCP server:* it would have to be configured in each agent. crosshall needs only a shell command.

## Documentation

| To find | Read |
| --- | --- |
| Every install question, the walkthrough with all output, deadlines, limits, removal | [Guide](docs/guide.md) |
| The member rules and guarantees an agent follows | [PROTOCOL.md](PROTOCOL.md) |
| Hooks, notices and the crosshall skill per agent | [Adapters](docs/adapters.md) |
| How an idle agent waits for work | [Waiting](docs/waiting.md) |
| State files, the loop guard, moving and removing | [Operations](docs/operations.md) |
| Joining from a second machine over SSH | [Linking](docs/linking.md) |
| Which agent builds each feature was verified on | [CONFORMANCE.md](CONFORMANCE.md) |
| Upgrade steps and release notes | [UPGRADING.md](UPGRADING.md), [CHANGELOG.md](CHANGELOG.md) |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md). Report security issues through [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE). Copyright 2026 Adrian Studio.
