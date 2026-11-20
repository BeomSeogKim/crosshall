# Claude Code and Codex CLI adapters

Adapters are optional hooks. crosshall works without them: a member that runs the commands itself follows the same rules.

## Setting up

In your own terminal, run `crosshall setup status` to check whether the Claude Code and Codex CLIs are on PATH. It shows each adapter's configuration path, hook installation state and policy, and reports any drift or pending recovery. Add `--json` before `setup` for machine-readable status. Antigravity supports session identity and has no hook adapter.

`crosshall setup codex` pins the selected crosshall state directory in its hook commands, so use that directory for member commands in the same `CODEX_HOME` and a separate `CODEX_HOME` for each independent state directory.

`crosshall setup` shows each vendor's plan and asks separately. The question shows the absolute settings path and how it was chosen. Claude Code uses `$HOME/.claude/settings.json` by default and Codex uses `$HOME/.codex/hooks.json`. To select another directory, set `CLAUDE_CONFIG_DIR` or `CODEX_HOME` to an absolute path. An empty or invalid override produces an error for that vendor. Claude offers separate read-command and send-command allow rules. In Codex, review and trust the installed hooks in the hook review screen.

To revisit one vendor, run `crosshall setup claude` or `crosshall setup codex`. An owner-run script can preview and apply the named vendors in two commands.

```sh
crosshall setup --dry-run claude codex
crosshall setup claude codex --apply
```

`--apply` reads no input. For a new setup, it installs only the named vendors' base hooks. If it can resume an interrupted setup for the same vendors, it applies the edits you already approved, including any Claude allow rules.

With input or output redirected, `crosshall setup claude` previews the plan, while bare `crosshall setup` prints the scripted form and exits with code 2.

## Joining a repository's channel

Hooks run for every session of that agent under your user account, in every repository. Run `crosshall owner channel default demo` in a repository to have new sessions there join the channel `#demo` at startup. If the project has no channel assigned, a new session gets a one-line prompt to join its team's channel with `crosshall join <channel>`. The join output shows its assigned `@alias` and the roster. A session that already joined keeps its channel.

## The turn-end check

An adapter only checks the turn-end rule; it does not replace it. When a turn ends while a request addressed to the session is unread, the adapter continues that turn once with the `[crosshall] pending:` line. It does so only if the check is available for this build and mode. Claude Code receives the line as Stop hook context. On Codex the check is unavailable in this release, because a stock Codex session cannot be verified. Each request is announced this way once and stays unread in the inbox. Each continuation costs one more model request. `crosshall owner doctor` shows per adapter whether the turn-end notice is available, and how many continuations it committed today. It is available only when three things hold. The owner has imported tested evidence for that exact build and mode, recorded by a conformance run that observed the continuation. The adapter policy is on. The running session's build and mode can be verified.

To make the check available for Claude Code in print mode (`claude -p`), work from the release checkout with `claude` and Go on PATH:
1. Create an output directory outside your home and resolve its physical path, so it contains no symbolic link: `out="$(cd "$(mktemp -d)" && pwd -P)"`.
2. Run `CROSSHALL_CONFORMANCE_OUT="$out" go test -tags conformance -count=1 -run '^TestClaudeCodeTurnEndConformance$' ./test/conformance`. The run uses temporary homes and a local stand-in for the model, so it makes no billed requests.
3. Import the file it writes to `"$out"` with `crosshall owner adapter evidence <file>`.
4. Turn the adapter on with `crosshall owner adapter set claude --enabled on`.

`crosshall owner doctor` then lists `turn_end tested` for that build. Its turn-end notice line reads available while a joined member runs a verified session of that build and mode. Its count is of committed continuation claims, not of confirmed model continuations. Evidence belongs to one hook manifest revision: after an upgrade that changes it (`claude-2` in this release), run the conformance test again and import the new file.

Interactive Claude Code sessions report a verified build and the mode `interactive`, but no conformance run has tested the check there, so it stays unavailable. Stock Codex sessions cannot be verified, so the check stays unavailable for them too.

## The prompt line

At a prompt, a joined session can see a `[crosshall] pending:` line with unread message IDs. Each ID is listed once: a prompt does not repeat what an earlier line, the turn-end check or a crosshall notice the session received already announced. After a compaction or `/clear`, work the session was already told about comes back as one count, `[crosshall] 2 unread, 0 owed. Run crosshall read.`, and after `/clear` the session is also told its alias and the command that prints the member rules. `crosshall who` shows the last observed event and its age. Codex observations carry the label `unverified` because the hook cannot verify the running version and mode. Nothing wakes an idle session.

## Turning adapters off and removing them

`crosshall setup off` disables both adapters and leaves the settings in place. Use `crosshall uninstall codex` or `crosshall uninstall claude` to remove one adapter. Bare `crosshall uninstall` covers both. Uninstall removes hooks only, not the binary or the state; see [removing crosshall](operations.md#removing-crosshall) for that. It asks before removing unchanged entries that crosshall added and reports entries it leaves for manual review.

## What doctor reports

For each vendor version, platform and mode, `crosshall owner doctor` reports capabilities as `tested` (passed a conformance check), `candidate` (awaiting verification) or `unavailable` (unsupported), with `unknown` when evidence is missing.

There is no reply reminder in this release; `crosshall setup --dry-run` and `crosshall owner doctor` list it as `parked`.

For each adapter, doctor reports recorded hook starts, helper timeouts and missed deadlines for the current UTC day. It shows `hook counters unknown` when it has no records for that adapter that day.
