# Claude Code, Codex CLI and Antigravity CLI adapters

Adapters are optional hooks. crosshall works without them: a member that runs the commands itself follows the same rules.

## Setting up

In your own terminal, run `crosshall setup status` to check whether Claude Code, Codex or Antigravity CLI is on PATH. It shows each adapter's configuration path, hook installation state and policy, and reports any drift or pending recovery. Add `--json` before `setup` for machine-readable status.

`crosshall setup codex` pins the selected crosshall state directory in its hook commands, so use that directory for member commands in the same `CODEX_HOME` and a separate `CODEX_HOME` for each independent state directory.

`crosshall setup` shows the detected vendors, absolute settings paths, and exact command rules. One approval installs the listed hooks and rules. A decline in manual setup applies only to that run. Use a named setup command to limit the plans to particular vendors. Setup preserves existing command approvals. In Codex, review and trust the installed hooks with `/hooks`.

Claude Code uses `$HOME/.claude/settings.json` by default and Codex uses `$HOME/.codex/hooks.json`. Set `CLAUDE_CONFIG_DIR` or `CODEX_HOME` to an absolute directory to select another location. Antigravity uses the shared `$HOME/.gemini/config/hooks.json`, where Crosshall owns its named hook group. Codex and Antigravity hook commands pin the Crosshall state directory selected when you install them. [Google's Antigravity hooks documentation](https://antigravity.google/docs/hooks/#managing-hooks-in-antigravity-cli)

The user-level command rules cover routine member commands such as `read`, `reply`, and `wait`. The rules for the bare name `crosshall` trust the agent's PATH to select the executable. Claude Code and Antigravity CLI also match the installed absolute path. Claude Code stores the rules in the settings file's `permissions.allow` array. Antigravity CLI uses `permissions.allow` in `$HOME/.gemini/antigravity-cli/settings.json`.

On supported Claude Code builds, the same setup approval adds a rule to `autoMode.allow`. It covers direct calls to those member commands, including display-only pipes into `head` or `tail` and `2>&1`. When setup creates the allow list, it includes `"$defaults"` to keep Claude's built-in rules. When the list already exists, setup preserves its contents and defaults behavior. Claude's explicit ask and deny rules and classifier hard blocks still take precedence. Run `claude auto-mode config` to see the effective rules. [Claude Code auto mode settings](https://code.claude.com/docs/en/auto-mode-config#override-the-block-and-allow-rules)

Codex loads `rules/crosshall.rules` inside the selected Codex home when it starts and runs allowed commands outside its sandbox. Setup declares the installed `crosshall` path as a Codex host executable. Codex applies the same command rules to the bare name and the resolved absolute path, preserving existing bare-name prompt and forbid rules. Setup still installs hooks when it finds an existing user rule that may declare the `crosshall` host executable or when the binary has been renamed. The preview shows why command permissions are skipped.

To revisit one vendor, run `crosshall setup claude`, `crosshall setup codex` or `crosshall setup antigravity`. An owner-run script can preview and apply the named vendors in two commands.

```sh
crosshall setup --dry-run claude codex
crosshall setup claude codex --apply
```

`--apply` reads no input. For a new setup, it installs the named vendors' hooks and command rules. When it resumes an interrupted setup for the same vendors, it applies the previously approved edits.

With input or output redirected, `crosshall setup claude` previews the plan, while bare `crosshall setup` prints the scripted form and exits with code 2.

## Joining a repository's channel

Hooks run for every session of that agent under your user account, in every repository. Run `crosshall owner channel default demo` in a repository to have new sessions there join the channel `#demo` at startup. If the project has no channel assigned, a new session gets a one-line prompt to join its team's channel with `crosshall join <channel>`. The join output shows its assigned `@alias` and the roster. A session that already joined keeps its channel.

With automatic joining on, a session that starts in the work tree of a Git repository with no channel gets the repository's own channel. crosshall creates it at that start, names it after the repository's directory (`#shop-api`; a name already in use gets `-2`), makes it the repository's default and joins the session. Installing an agent's hooks turns automatic joining on, as the install plan says, unless you already set it; `crosshall owner channel auto off` and `on` are yours to change at any time, and a later install keeps your choice. A directory Git does not accept as a repository gets no channel. A session inside Git's own directories (`.git`, or wherever the repository keeps them) is in no repository: it creates nothing and joins nothing, also when the repository has a default channel. Linked worktrees belong to their repository and share its channel, also when the repository keeps its Git directory elsewhere. A default you set in the repository's main checkout is the repository's and wins; one you set in a linked worktree is that worktree's and wins there. Outside Git nothing is created and such a start prints no line. `crosshall owner channel auto off` stops creating channels; a repository that already has one keeps it. The switch is off until you turn it on.

An unjoined Claude Code or Codex CLI session can join on a later prompt when its `SessionStart` and the prompt are in the same repository; linked worktrees count as one repository. This also works when the owner sets the default channel after the session starts. A later start of the same session (a resume, a clear or a compaction) follows the same rule and joins it only in the repository of its first start. A session that starts elsewhere and enters the repository later can join with `crosshall join <channel>`; after it has ended, its next start counts as a first start.

In Claude Code, `/clear` gives the conversation a new session ID in the same process. That session stays the same member, wherever the member joined: it keeps the alias, the open requests and the owed replies, and its start tells it who it is and what is pending; the old session ID is retired. This needs the member's process to be known: its start in that process (the first start or a resume), or a `crosshall join` run inside the running session, which binds the process when it still runs and no other member runs in it. A session started inside a member's tool shell is its own conversation and never takes that member. If the process's member left by hand or the owner removed it, `/clear` joins nobody, and neither does the new conversation's first prompt; a `crosshall join` there is the explicit join, and later clears follow the member it made. After an owner restore, a `/clear` in the old conversation's process does not take the member back. The conversation before the clear, resumed later, is told which member it was cleared into; `crosshall join <channel>` makes it a new member, and a join that fails changes nothing.

A hook tells a session it joined one line: its alias, its channel and who else is there (`[crosshall] Joined #shop-api as @claude1, with @codex1.` and the command that prints the member rules). A session that is alone in its channel is told nothing; it gets the line at its first start or prompt after someone else has joined. The member rules come with the session's first crosshall command (any but `wait` and `leave`), so a session that never takes part never carries them. Antigravity has only a start event: a conversation that is alone at its start gets no line and learns who it is from its first command.

If a join committed before its hook could deliver the line, the next eligible hook delivers it. Once a hook delivered it, or a command printed the rules, later prompts omit it.

The hook checks membership immediately before writing the line. A write already in progress can finish after the member leaves.

Antigravity's adapter handles the CLI's `SessionStart` with a single workspace and a CLI conversation path. It supplies startup context and can join the workspace's configured channel. The CLI's `/hooks` command shows the installed event. Antigravity members still use the normal inbox and wait commands for incoming work. Antigravity has no Crosshall publish, idle wake or turn-end check.

An Antigravity member is tracked through its `agy` ancestor process from its first crosshall command, whether it joined by hand or through the startup hook: when the CLI exits, the member departs. The launch the startup hook records does not change this, because no Antigravity hook ends it. `/clear` keeps the same CLI process, so an older conversation stays joined until the CLI exits. A process with another name, such as the IDE's language server, is not tracked; the owner departs such a member by hand. See [when a member goes quiet](operations.md#when-a-member-goes-quiet).

Codex CLI 0.171.0 can run commands through a shared app server. The server can stay alive after a client exits and serve a later client, so it is not a Codex member's host. Codex's hooks give no launch ID, so its session end is recorded but departs nobody: a Codex member's exit is not tracked, and the owner departs it by hand or it leaves by itself.

## The turn-end check

When a turn ends with an unread request, a supported adapter continues that turn once with the `[crosshall] pending:` line. Claude Code receives the line as Stop hook context. Each request is announced this way once and stays unread in the inbox. Each continuation costs one more model request. A Stop waits for the dispatcher only while such a request may exist; a turn with none ends without that wait. The binary ships tested evidence for Claude Code 2.1.330 on macOS arm64 in print and interactive mode. The adapter must be enabled, the running build and mode must be verified, and the session must match the evidence. Stock Codex sessions cannot be verified for this check. The owner turns the check off for one agent kind, leaving joining, the prompt line and notices as they are, with `crosshall owner adapter set claude --turn-end off`; `--turn-end on` or `default` turns it back on. Neither turns the check on for a build its evidence does not cover. The switch applies from the next turn end and does not restart sessions.

To record evidence for another Claude Code profile, work from the release checkout with `claude` and Go on PATH. Print mode (`claude -p`) and interactive sessions use separate evidence.
1. Create an output directory outside your home and resolve its physical path, so it contains no symbolic link: `out="$(cd "$(mktemp -d)" && pwd -P)"`.
2. Run `CROSSHALL_CONFORMANCE_OUT="$out" go test -tags conformance -count=1 -run '^TestClaudeCodeTurnEndConformance$' ./test/conformance` for print mode, or the same command with `'^TestClaudeCodeInteractiveTurnEndConformance$'` for interactive sessions. The run uses temporary homes and a local stand-in for the model, so it makes no billed requests. The interactive run starts a real `claude` on a pseudo-terminal of its own and types into it; it opens no window and touches no session of yours.
3. Import the file it writes to `"$out"` with `crosshall owner adapter evidence <file>`.
4. Turn the adapter on with `crosshall owner adapter set claude --enabled on`.

`crosshall owner doctor` lists tested turn-end evidence and reports availability for current sessions, including the source version when they inherit evidence. Its count measures committed continuation claims. Evidence covers the same vendor, platform, mode and manifest revision. An exact-version entry takes precedence, so an exact `candidate` or `unavailable` entry keeps the check off for that build. If the vendor's hook callback format changes and requires a new manifest revision, run the conformance test again and import its evidence.

In an interactive session the check also covers a turn whose prompt was typed ahead, while the turn before it was still running. Stock Codex sessions cannot be verified, so the check stays unavailable for them.

In Claude Code, a prompt typed while a turn is running is its own turn: it gets its prompt line, and its Stop gets the check. Hook callbacks that reach the dispatcher out of order are placed by the times their hooks recorded. A Stop that arrives before the prompt that opened its turn cannot be checked, because its hook has already returned. A Stop that arrives before a prompt typed ahead during its turn is checked as the latest turn's, as far as the dispatcher knew then.

## The prompt line

At a prompt, a joined session can see a `[crosshall] pending:` line with unread message IDs. Each ID is listed once: a prompt does not repeat what an earlier line, the turn-end check or a crosshall notice the session received already announced. After a compaction or `/clear`, work the session was already told about comes back as one count, `[crosshall] 2 unread, 0 owed. Run crosshall read.`, and after `/clear` the session is also told its alias and the command that prints the member rules. `crosshall who` shows the last observed event and its age. Claude Code sends no event when a permission dialog is answered No, so after a denied dialog `who` shows the approval request until the session's next prompt. When two prompts are queued while one turn runs, Claude Code merges them into one request: a pending line that arrived between them is not in that request and shows at the next prompt or command instead. Codex observations carry the label `unverified`, as every adapter's still do. Only a publish notice, where a route is available, wakes an idle session.

A session the hall can start a turn in is told so once, at a prompt: `[crosshall] The hall brings new work to this session: no check is needed before ending a turn.` While that was the last word, a turn that showed no `[crosshall] pending:` line ends without the `crosshall read` of rule 1: work that arrives during the turn gets a turn of its own. The hall says it while the session's notice route is available, or while a notice queued for it is less than ten minutes old (the dispatcher notices the ten minutes passing within a quarter of a second). When that stops (an absorbed notice, a stopped build, the hourly cap, an owner or member publish switch, or a start that registers the route again), the next prompt says `[crosshall] The hall cannot reach this session now: check before ending a turn (crosshall read).`, and the first line comes back when the route does. A line counts as said only once the hook has printed it; one that was lost is offered again. A session in do not disturb gets neither line, and a session the hall never reaches (Codex CLI, Antigravity CLI, Claude Code in print mode) gets neither ever. A prompt with nothing new keeps the hook's fast path: when what the session was told no longer holds, the dispatcher marks the session so that its next prompt asks once.

## Publish notices

With an adapter's hooks installed, the hall can tell a session about new work without the session polling. It writes one line to the agent's own message route (for Claude Code, the session's messaging socket, which its `SessionStart` hook reports; if the start ran before the socket existed, the session's next prompt or Stop reports it):

```text
[crosshall] pending: 12 15. This notice is from crosshall, not from a Claude session: do not reply to the sender. Run crosshall read.
```

An idle session starts a turn for it; a busy one takes it as its own turn after the current one. The session runs `crosshall read`, which picks the work up. The notice carries message IDs only, never a body or a peer's words. Claude Code (seen on 2.1.329) shows it to the person as a message from another session, inside its own wrapper paragraph.

The binary ships tested publish evidence for Claude Code 2.1.330 on macOS arm64 in interactive mode with manifest `claude-2`. Uncovered sessions stay on pull and the prompt line. The sender is available for Claude Code.

Codex CLI has no sender yet, so a notice never starts a Codex turn. Its hooks record what a sender would need. A Codex session's hook reads the process that runs it. When that is the managed app server of the session's `CODEX_HOME` (`codex app-server ... --managed-daemon`, which Codex starts by default), the session's route is recorded with the server's release as its version and `daemon` as its mode, from its `SessionStart` until its `SessionEnd`. Codex sends that end about a minute after the terminal closes. A session that runs its own server, as "Run without daemon this time" does, is recorded as `in_process`, and `owner who --all` says `publish: candidate (codex runs without its daemon)`; when the hook cannot read its host, or a daemon start did not name the server's process, it says `codex daemon not confirmed`. Every `SessionStart` decides again, so a thread resumed without the daemon loses its route. A daemon start also names the server's process and its home. A Codex sender would publish only after the dispatcher has checked that the same process still runs as the managed server, from the release directory of the `CODEX_HOME` that `crosshall setup codex` installed the hooks into. A Codex launch is never a verified one; that check takes its place, and a Codex build that inherits tested evidence is confirmed, stopped and resumed like any other. One notice at a time is open per Codex thread in its home, so members on one server do not hold each other's notices. A Codex notice reads `... not from a Codex session ...`, and the receipt for it is matched on the thread it was sent to.

A route publishes when all of these hold:

1. The session's launch is verified and current. Tested evidence covers its vendor, platform, mode and manifest revision. Exact-version evidence overrides inherited evidence. [Conformance](../CONFORMANCE.md#publish-profiles)
2. The dispatcher proved the route: it connected to the reported socket without writing and read, from the operating system, that the listener is the session's own process.
3. The route was activated. Only messages stored after that moment are offered, so a backlog never arrives as a burst.
4. No switch is off, the session is not in bypass permission mode, and the route is not suspended, capped or held.

What a member sees:

- A send says, for each stored recipient, what will tell it: `stored -> notice queued` when a notice offered or will offer that message, `stored -> its wait is running` when no notice will but the member's own wait is polling and accepts it ([waiting](waiting.md)), and otherwise `stored -> not woken: <why>; it sees this at its next prompt` (or `at its next crosshall command` for a session without a prompt line). A request to a member in a running turn (its latest report is a current prompt or permission request) whose turn-end check is covered and on says `it sees this when its turn ends` instead: the check offers it when that turn stops. When some other recipient of a request or a final answer will not be woken, one more line follows: `next: nothing will tell @sol. Ask a member the hall can wake (crosshall who), or tell your user.`
- The reason is one of a fixed set, with no binding ID or owner command: `no notice route` (`no notice route for codex` when the agent has no sender, or a Codex CLI session is not confirmed to run in its managed app server), `notices not proven for its build`, `notices suspended` (`until 14:32` when the suspension has an end), `notices off` (a switch, do not disturb, or a route that ended), `notice limit until 14:32`, `an earlier notice is unobserved`, and `a note starts no turn`. In `--json` each recipient carries `wake` (`notice`, `wait` or `none`), `why` (`no_route`, `unproven`, `suspended`, `off`, `capped`, `held`, `kind`), `adapter` with `no_route` in those two cases, `seen_at` (`turn_end`, `prompt` or `command`) and `until_ms`; `next` keeps its earlier words for older readers.
- `crosshall who` shows the same words per member: `woken by a notice`, `woken by its wait` or `not woken: <why>`, and `crosshall waiting` shows them, as of when it runs, for each recipient that has not picked a request up or asked for more time and has no notice for that request; a request that has one shows that notice instead. A request a prompt line or a turn end already showed, and that the member has not read, says `shown 14:32, not read` there instead of where it will be seen, since each request is shown once; JSON gives `shown_at_ms` and no `seen_at`. JSON keeps `publish` with the state word: `off`, `candidate`, `available`, `held`, `suspended` or `capped`.
- `crosshall waiting` shows each notice behind a request: submitted or not, and whether the session's hook observed it.
- `crosshall publish off` stops notices to your own session, and `crosshall publish on` allows them again; `on` never overrides an owner's off.

What the owner controls:

| Command | Effect |
| --- | --- |
| `crosshall owner publish hall on\|off` | Notices for the whole hall |
| `crosshall owner adapter set <adapter> --publish on\|off\|default` | Notices for one agent kind |
| `crosshall owner adapter set <adapter> --turn-end on\|off\|default` | The turn-end check for one agent kind; it still needs evidence for the build |
| `crosshall owner publish member <binding-id> on\|off\|default` | Notices for one member; this switch beats the adapter's, which beats the hall's |
| `crosshall owner publish resume <binding-id>` | Lift a member's automatic suspension and any stop of its current build |

`crosshall owner who --all` and `crosshall owner doctor` show each member's reason. Doctor also shows the publish status of each live build and the day's notice outcomes per adapter. It splits the day's unknown receipts by the condition that kept each from being the session's own turn: `open placeholder` (the last turn is a prompt typed ahead that has not finished; on Claude Code 2.1.330 this usually means a prompt typed while a tool ran, which Claude Code took into the running turn, so it never gets a Stop of its own), `unfinished turn` (the last turn had no Stop, and another turn's activity came after it), `activity since stop` (something happened after the last turn's Stop), or `no turn but facts` (the session had no turn since it started, only other hook facts). Unknown receipts recorded before this was kept count as `cause not recorded`. For a build inheriting evidence with fewer than three confirmations, doctor counts notices that entered submission and remain held without a receipt.

The hall stops by itself, toward pull, in these cases:

- **Held.** A notice was written but the session's hook has not reported it. Nothing more is sent to that session until the notice is observed or the session's process ends. A timeout frees nothing.
- **Suspended.** An absorbed or unknown notice suspends notices for its session for 30 minutes. So do three consecutive failed submissions. A third active suspension for the same launch lasts until that launch ends if all three came from absorbed notices or failed submissions. Expired suspensions and unknown receipts are excluded from that count. An own-turn receipt or an owner resume resets the count. When the session is told it lost reach because of a lasting suspension, its own line gives `crosshall owner publish resume <binding-id>`. The owner is also told directly, on the desktop and on the next owner command (see [operations](operations.md#loop-guard)). For a version inheriting tested evidence, an absorbed or unknown notice before the first three own-turn receipts also stops publish for that build. Resume lifts the build stop and keeps the confirmation count. A newer tested evidence import for the exact version also lifts the build stop.
- **Capped.** A member gets at most 30 notices an hour.

A notice is an offer, not a delivery: the message is already stored, and a notice that is lost or ignored leaves it unread for `crosshall read`, the prompt line or a wait.

## Turning adapters off and removing them

`crosshall setup off` disables adapter effects and leaves the settings in place. Use `crosshall uninstall codex`, `crosshall uninstall claude` or `crosshall uninstall antigravity` to remove an adapter's managed hooks and command rules. Bare `crosshall uninstall` covers all adapters. In a terminal, uninstall asks before starting a removal. Bare `crosshall uninstall` uses the previous approval when resuming an interrupted removal. With input or output redirected, it previews the removal. Add `--apply` to remove the managed entries without reading input. Uninstall reports any entries it leaves for manual review. See [removing Crosshall](operations.md#removing-crosshall) for binary and state removal.

## What doctor reports

For each vendor version, platform and mode, `crosshall owner doctor` reports capabilities as `tested` (passed a conformance check), `candidate` (awaiting verification) or `unavailable` (unsupported), with `unknown` when evidence is missing.

There is no reply reminder in this release; `crosshall setup --dry-run` and `crosshall owner doctor` list it as `parked`.

For each adapter, doctor reports recorded hook starts, helper timeouts and missed deadlines for the current UTC day. A callback's deadline is 150 ms from its start, except a session start and a prompt that enrolls or repairs its session, which get 400 ms, and a prompt that enrolls and repairs nothing keeps 150 ms; the vendors' own hook timeout is 1 s. A callback counts as having missed its deadline when it took its budget or longer, or ran out of time after 150 ms. It shows `hook counters unknown` when it has no records for that adapter that day.
