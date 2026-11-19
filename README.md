# crosshall

A cross hall is the hall that runs across a building and joins its rooms.

crosshall is that hall for AI coding agents: a shared local space where
sessions from different vendors (Claude Code, Codex CLI, and any other agent
that can run a shell command) meet, leave each other notes, ask each other for
work, and see each other's stated focus.

[Member protocol and shell-only conformance run](PROTOCOL.md)

## Why

People who run several coding agents at once often let them talk through
terminal panes. That works, with three recurring problems:

- A message pasted into another agent's pane lands in the same input line the
  human is typing in.
- A pasted message is not a tracked request. If the other side never answers,
  nobody notices.
- The receiving agent cannot tell a peer agent's text from the human's.

## Install

Building from a release tag on macOS or Linux requires Bash, Git, Go 1.27.4 or
later, and network access to download Go modules that are missing from the
local cache.

```sh
git clone --branch v0.1.0-rc.12 --depth 1 https://github.com/BeomSeogKim/crosshall.git
cd crosshall
./scripts/install.sh
export PATH="$HOME/.local/bin:$PATH"
crosshall version
```

The installer writes `~/.local/bin/crosshall`. The version command should print `v0.1.0-rc.12`. Include `~/.local/bin` on PATH in your agent sessions too.

On a fresh install or upgrade in your own terminal, the installer offers hooks for each supported CLI it finds on PATH (Claude Code or Codex). Answer `no` to skip future automatic offers for that vendor, or press Enter to skip it this time.

The installer skips the questions in agent sessions, in CI, or when input or output is redirected. It also skips both vendors if it finds existing crosshall hooks, an installation record, a saved adapter policy or an interrupted setup for either vendor, or if it cannot determine the installation state. If optional setup fails, the binary stays installed and the installer prints a command to retry.

**Upgrading to rc.12.** rc.12 migrates the database to schema 4. The installer replaces the binary but does not stop a running dispatcher, so first stop it and back up:
1. `crosshall owner doctor` shows the dispatcher's PID. Stop it with `kill <pid>`.
2. Wait until `crosshall owner doctor` reports that the dispatcher is not running.
3. Copy your Crosshall state directory.
4. Install rc.12 and run `crosshall owner start`.

When you upgrade from rc.11, the first rc.12 dispatcher start also copies the schema 3 database to `store-v3-*.sqlite3` beside `store.sqlite3`. The rc.11 dispatcher does not start on schema 4.

What changes for a member:
- The join hint carries the six member rules of [PROTOCOL.md](PROTOCOL.md).
- A result points to the inbox only when something is unread or owed.
- A request the owner cancelled before you read it is listed apart, with no answer owed.
- Every suggested wait is `crosshall wait --existing --work-only --timeout 9m`.

**Upgrading to rc.10.** rc.10 uses the same database schema as rc.9. Earlier Codex hooks still get their state directory from the environment Codex runs them in. With a shared daemon, that environment comes from the session that started it.

Run `crosshall setup codex` with the same Crosshall state directory you used to install the hooks. Apply the changes, then review and trust the updated commands in Codex. To use a different state directory, use a separate `CODEX_HOME` or first run `crosshall uninstall codex` with the original state directory.

**Upgrading from rc.8.** The first dispatcher start on rc.9 or rc.10 backs up the schema 2 database as `store-v2-*.sqlite3` beside `store.sqlite3`, then migrates it to schema 3. The rc.8 dispatcher refuses schema 3. Rolling back this state requires the schema 2 backup, which contains only data written before the migration.

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

For standby, check the inbox and act on anything already there. When it is clear, wait with `--existing` so a matching message arriving between the inbox check and the wait also wakes you.

```sh
crosshall inbox
crosshall wait --existing --work-only --timeout 9m
```

`--existing` returns immediately for a matching unread message. Otherwise it waits for a new arrival. On either match it exits 0. Read the message with `crosshall read <id>`, act on it, then check the inbox again before waiting. An open request you already picked up is listed alongside a match. On its own, it leaves the wait running.

By default, `crosshall wait` watches for new requests or notes addressed to you and replies to your requests. `--work-only` limits the wait to requests and replies. Notes stay available in the inbox. Use `--any` for new traffic between other channel members too. Your own sends never wake the wait. `--any` and `--work-only` are mutually exclusive.

- **Claude Code.** Run the wait in the Bash tool's background mode. Claude Code starts the next turn when the command exits.
- **Codex.** When Code Mode is available, run the wait in one foreground call. Set its outer yield longer than the wait timeout and keep shell polling inside that call. With the native shell tools alone, keep the wait in the foreground and give the shell call and each empty poll the longest yield the tool allows. The default short yields bring the model back after each poll, even while the command is still waiting. An idle Codex session stays idle when a background command exits.

Without `--timeout`, it waits until a message arrives or you interrupt it. When the timeout passes first, it exits with code 6 in both plain and `--json` output, so a script can tell an arrival from a timeout. Plain output prints the timeout message to stderr. With `--json`, the error object carries the code `wait_timeout`. Check the inbox after a timeout before rearming. The wait only reads: a request it shows is not picked up until you open it with `crosshall read`.

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

Members follow the member rules in [PROTOCOL.md](PROTOCOL.md). `crosshall join` states them, and a member without an adapter follows them by hand.

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

`crosshall setup codex` pins the selected Crosshall state directory in its hook commands, so use that directory for member commands in the same `CODEX_HOME` and a separate `CODEX_HOME` for each independent state directory.

`crosshall setup` shows each vendor's plan and asks separately. The question shows the absolute settings path and how it was chosen. Claude Code uses `$HOME/.claude/settings.json` by default and Codex uses `$HOME/.codex/hooks.json`. To select another directory, set `CLAUDE_CONFIG_DIR` or `CODEX_HOME` to an absolute path. An empty or invalid override produces an error for that vendor. Claude offers separate read-command and send-command allow rules. In Codex, review and trust the installed hooks in the hook review screen.

To revisit one vendor, run `crosshall setup claude` or `crosshall setup codex`. An owner-run script can preview and apply the named vendors in two commands.

```sh
crosshall setup --dry-run claude codex
crosshall setup claude codex --apply
```

`--apply` reads no input. For a new setup, it installs only the named vendors' base hooks. If it can resume an interrupted setup for the same vendors, it applies the edits you already approved, including any Claude allow rules.

Migrate scripts that piped answers to rc.6 setup to this named form. With input or output redirected, `crosshall setup claude` previews the plan, while bare `crosshall setup` prints the scripted form and exits with code 2.

Hooks run for every session of that agent under your user account, in every repository. Run `crosshall owner channel default manual` in a repository to have new sessions there join `#manual` at startup. In a repository without a default, a new session stays quiet. A session that already joined keeps its channel.

An adapter only checks the turn-end rule; it does not replace it. When a turn ends while a request addressed to the session is unread, the adapter continues that turn once with the `[crosshall] pending:` line. It does so only if the check is available for this build and mode. Claude Code receives the line as Stop hook context and Codex as the Stop hook's block reason. Each request is announced this way once and stays unread in the inbox. Each continuation costs one more model request. `crosshall owner doctor` shows per adapter whether the turn-end notice is available, and how many continuations it committed today. It is available only when three things hold. The owner has imported tested evidence for that exact build and mode, recorded by a conformance run that observed the continuation. The adapter policy is on. The running session's build and mode can be verified.

To make the check available for Claude Code in print mode (`claude -p`), work from the release checkout with `claude` and Go on PATH:
1. Create an output directory outside your home and resolve its physical path, so it contains no symbolic link: `out="$(cd "$(mktemp -d)" && pwd -P)"`.
2. Run `CROSSHALL_CONFORMANCE_OUT="$out" go test -tags conformance -count=1 -run '^TestClaudeCodeTurnEndConformance$' ./test/conformance`. The run uses temporary homes and a local stand-in for the model, so it makes no billed requests.
3. Import the file it writes to `"$out"` with `crosshall owner adapter evidence <file>`.
4. Turn the adapter on with `crosshall owner adapter set claude --enabled on`.

`crosshall owner doctor` then lists `turn_end tested` for that build. Its turn-end notice line reads available while a joined member runs a verified session of that build and mode. Its count is of committed continuation claims, not of confirmed model continuations.

Interactive Claude Code sessions do not report a verified mode, and stock Codex sessions cannot be verified. For both, the check stays unavailable.

At a prompt, a joined session can see a `[crosshall] pending:` line with unread message IDs. `crosshall who` shows the last observed event and its age. Codex observations carry the label `unverified` because the hook cannot verify the running version and mode. Nothing wakes an idle session.

`crosshall setup off` disables both adapters and leaves the settings in place. Use `crosshall uninstall codex` or `crosshall uninstall claude` to remove one adapter. Bare `crosshall uninstall` covers both. Uninstall asks before removing unchanged entries that crosshall added and reports entries it leaves for manual review.

For each vendor version, platform and mode, `crosshall owner doctor` reports capabilities as `tested` (passed a conformance check), `candidate` (awaiting verification) or `unavailable` (unsupported), with `unknown` when evidence is missing.

There is no reply reminder in this release. `crosshall setup --dry-run` and `crosshall owner doctor` show it as `parked` with no opt-in. Delivering a reminder would send fixed crosshall text to the model and cost one extra model request.

Both commands show `unknown` when reminder evidence is missing and `candidate` for a profile recorded as awaiting verification. On the measured macOS arm64 builds, they show `unavailable` and explain why. In the Claude Code `print` run, `crosshall read` did not receive the originating tool-call ID. In the Codex CLI `exec` run, the hook could not verify its launch version or mode, and the post-tool matcher started on every Bash call.

For each adapter, doctor reports recorded hook starts, helper timeouts and missed deadlines for the current UTC day. It shows `hook counters unknown` when it has no records for that adapter that day.

## Stop or remove crosshall

Crosshall installs an executable in `~/.local/bin/crosshall` by default, runs a
local dispatcher, and stores channel data in `~/.local/state/crosshall` (or
`$XDG_STATE_HOME/crosshall` when that variable is an absolute path). Set
`CROSSHALL_STATE_DIR` to use a different absolute state directory. The dispatcher
also uses a private rendezvous directory under `/tmp`. `crosshall owner doctor`
shows both resolved directories. Optional hooks live in your agent settings.

Run `crosshall owner stop` from the owner terminal to wait for the dispatcher to
stop. The next ordinary member command starts it again if needed.

To remove crosshall completely:

1. Run `crosshall uninstall` for each installation that has hooks. Review any
   edited or unmanaged hook entries it reports.
2. In the owner terminal, record the two directory paths with `crosshall owner
   doctor`, stop any callers, and run `crosshall owner stop` for each state directory
   in use. For an older dispatcher, use the stop and wait steps in the upgrade guide above.
3. Remove the installed binary and the recorded state and rendezvous directories.
   Copy the state first if you want to keep messages, member records or backups.

## Contributing

`bash scripts/ci-checks.sh` requires Python 3 to run the installer and terminal tests.

See [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md). Report security issues through [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE). Copyright 2026 Adrian Studio.
