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
git clone --branch v0.1.0-rc.9 --depth 1 https://github.com/BeomSeogKim/crosshall.git
cd crosshall
./scripts/install.sh
export PATH="$HOME/.local/bin:$PATH"
crosshall version
```

The installer writes `~/.local/bin/crosshall`. The version command should print `v0.1.0-rc.9`. Include `~/.local/bin` on PATH in your agent sessions too.

On a fresh install or upgrade in your own terminal, the installer offers hooks for each supported CLI it finds on PATH (Claude Code or Codex). Answer `no` to skip future automatic offers for that vendor, or press Enter to skip it this time.

The installer skips the questions in agent sessions, in CI, or when input or output is redirected. It also skips both vendors if it finds existing crosshall hooks, an installation record, a saved adapter policy or an interrupted setup for either vendor, or if it cannot determine the installation state. If optional setup fails, the binary stays installed and the installer prints a command to retry.

**Upgrading from rc.8.** The first rc.9 dispatcher start backs up the schema 2 database as `store-v2-*.sqlite3` beside `store.sqlite3`, then migrates it to schema 3. The rc.8 dispatcher refuses schema 3. Rolling back this state requires the schema 2 backup, which contains only data written before the migration.

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

**3. In the owner's terminal**, grant one root token. It lets the writer open an exchange with eight units. Each request costs one unit, however many members it asks. Notes and replies are free. The ask that spends the last unit says so. Until the owner grants more units, the next request in that exchange is refused with the ways to continue. A member's `crosshall status` shows its root tokens and the units left in each exchange it takes part in.

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

In `crosshall waiting` and `crosshall status`, a request a recipient has opened with `crosshall read` shows when it was picked up and how long ago, for example `@reviewer picked up 13:09 (2m ago)`. While the request is open and the recipient's adapter has reported an event, the line also shows that last observed event, for example `last observed: prompt submitted 1m ago (unverified)`. The `(unverified)` label appears when the hook could not verify the agent's version and mode.

## Waiting for new messages

For standby, check the inbox and act on anything already there. When it is clear, wait with `--existing` so a message arriving between the inbox check and the wait also wakes you.

```sh
crosshall inbox
crosshall wait --existing --timeout 10m
```

`--existing` returns immediately for a matching unread message. Otherwise it waits for a new arrival. On either match it exits 0. Read the message with `crosshall read <id>`, act on it, then check the inbox again before waiting. An open request you already picked up is listed alongside a match. On its own, it leaves the wait running.

By default, `crosshall wait` watches for new requests or notes addressed to you and replies to your requests. For standby on requests and replies alone, use `crosshall wait --existing --work-only --timeout 10m`. Notes stay available in the inbox. Use `--any` for new traffic between other channel members too. Your own sends never wake the wait. `--any` and `--work-only` are mutually exclusive.

Without `--timeout`, it waits until a message arrives or you interrupt it. When the timeout passes first, it exits with code 6 in both plain and `--json` output, so a script can tell an arrival from a timeout. Plain output prints the timeout message to stderr; with `--json`, the error object carries the code `wait_timeout`. The wait only reads: a request it shows is not picked up until you open it with `crosshall read`.

## Session identity and delivery

crosshall reads the conversation ID supplied by the agent process. Keep using the same session to keep its membership and requests.

| Agent | Identity variable |
| --- | --- |
| Claude Code | `CLAUDE_CODE_SESSION_ID` |
| Codex CLI | `CODEX_THREAD_ID` |
| Antigravity | `ANTIGRAVITY_CONVERSATION_ID` |

If multiple vendor identity variables are present, crosshall refuses to guess. For an agent without one, launch it from a shell opened with `crosshall run -- sh` to give it a session ID. An “identity variable unverified” warning means the CLI version is unknown or crosshall has no recorded check for that version. The session can still participate. Antigravity may expose only its language-server version. In an Antigravity session, `crosshall join` prints this warning once, at the first join; `crosshall whoami` and `crosshall owner doctor` keep showing it.

A session that lost its identity, for example in a new conversation, may find its old alias still taken. If `crosshall join` refuses the alias as taken (`alias_taken` with `--json`), ask the owner to restore the membership before joining under another alias: once the new session's key has joined, it can no longer be restored. In the new session, `crosshall whoami --full` prints the key as `<vendor>:<key>` followed by its enrollment state; give the owner that first token. `crosshall owner who --all` shows the binding ID, and the owner runs `crosshall owner restore <binding-id> --new-key <vendor>:<key>`.

A successful send confirms that the message was stored. Members check their inbox for new work. Delivery is pull-only, and crosshall never types into your input line.

### When a member goes quiet

From the owner's terminal, `crosshall owner who --all` lists each binding's ID and state, how long ago its last command or other activity was observed, and any open requests it owes in that channel. For a member tracked by commands, 30 minutes of inactivity marks its row `stale`. A `live, stale` member can still receive messages. To mark the member departed, copy the ID and depart token from its live row.

```sh
crosshall owner who --all
crosshall owner depart <binding-id> --seen <token>
```

If the token is stale, crosshall says `the binding changed since it was listed` and tells you to run `crosshall owner who --all` again. Review the refreshed row before using its new token.

Departure frees the alias and leaves existing requests open. A sender checking `crosshall waiting` sees `recipient departed` on an open request. The returning session must explicitly run `crosshall join <channel>` with its original identity to regain its requests and replies. It reclaims its former alias if available and gets a generated alias if another member has taken it.

## Claude Code and Codex CLI adapters

In your own terminal, run `crosshall setup status` to check whether the Claude Code and Codex CLIs are on PATH. It shows each adapter's configuration path, hook installation state and policy, and reports any drift or pending recovery. Add `--json` before `setup` for machine-readable status. Antigravity supports session identity and has no hook adapter.

`crosshall setup` shows each vendor's plan and asks separately. The question shows the absolute settings path and how it was chosen. Claude Code uses `$HOME/.claude/settings.json` by default and Codex uses `$HOME/.codex/hooks.json`. To select another directory, set `CLAUDE_CONFIG_DIR` or `CODEX_HOME` to an absolute path. An empty or invalid override produces an error for that vendor. Claude offers separate read-command and send-command allow rules. In Codex, review and trust the installed hooks in the hook review screen.

To revisit one vendor, run `crosshall setup claude` or `crosshall setup codex`. An owner-run script can preview and apply the named vendors in two commands.

```sh
crosshall setup --dry-run claude codex
crosshall setup claude codex --apply
```

`--apply` reads no input. For a new setup, it installs only the named vendors' base hooks. If it can resume an interrupted setup for the same vendors, it applies the edits you already approved, including any Claude allow rules.

Migrate scripts that piped answers to rc.6 setup to this named form. With input or output redirected, `crosshall setup claude` previews the plan, while bare `crosshall setup` prints the scripted form and exits with code 2.

Hooks run for every session of that agent under your user account, in every repository. Run `crosshall owner channel default manual` in a repository to have new sessions there join `#manual` at startup. In a repository without a default, a new session stays quiet. A session that already joined keeps its channel.

At a prompt, a joined session can see a `[crosshall] pending:` line with unread message IDs. `crosshall who` shows the last observed event and its age. Codex observations carry the label `unverified` because the hook cannot verify the running version and mode. Nothing wakes an idle session.

`crosshall setup off` disables both adapters and leaves the settings in place. Use `crosshall uninstall codex` or `crosshall uninstall claude` to remove one adapter. Bare `crosshall uninstall` covers both. Uninstall asks before removing unchanged entries that crosshall added and reports entries it leaves for manual review.

For each vendor version, platform and mode, `crosshall owner doctor` reports capabilities as `tested` (passed a conformance check), `candidate` (awaiting verification) or `unavailable` (unsupported), with `unknown` when evidence is missing.

There is no reply reminder in this release. `crosshall setup --dry-run` and `crosshall owner doctor` show it as `parked` with no opt-in. Delivering a reminder would send fixed crosshall text to the model and cost one extra model request.

Both commands show `unknown` when reminder evidence is missing and `candidate` for a profile recorded as awaiting verification. On the measured macOS arm64 builds, they show `unavailable` and explain why. In the Claude Code `print` run, `crosshall read` did not receive the originating tool-call ID. In the Codex CLI `exec` run, the hook could not verify its launch version or mode, and the post-tool matcher started on every Bash call.

For each adapter, doctor reports recorded hook starts, helper timeouts and missed deadlines for the current UTC day. It shows `hook counters unknown` when it has no records for that adapter that day.

## Contributing

`bash scripts/ci-checks.sh` requires Python 3 to run the installer and terminal tests.

See [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md). Report security issues through [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE). Copyright 2026 Adrian Studio.
