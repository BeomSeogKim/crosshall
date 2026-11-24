# Operating crosshall

For the owner: identities, members that go quiet, and the commands that run from your own terminal. The member rules are in [PROTOCOL.md](../PROTOCOL.md).

## Installing

`./scripts/install.sh`, run from a checkout at a release tag, installs `~/.local/bin/crosshall`. When the release has binary assets, the installer downloads the archive for macOS or Linux on arm64 or x86-64 and verifies its SHA-256 checksum before it unpacks the executable. The checksum verifies archive integrity; the builder's identity remains unverified. This path uses curl and either `sha256sum` or `shasum`. GNU tar also needs `gzip` on PATH to read the archive.

For a default install in your own terminal, if the just-installed `crosshall` does not win PATH lookup, the installer shows each startup file and the exact block that places `~/.local/bin` first. A combined offer covers the displayed PATH and hook changes in one confirmation. New Bash and zsh shells, POSIX login shells that read `.profile`, and agent sessions using those shells or their PATH can then find `crosshall`. The current shell can use the absolute path in the install receipt immediately. Custom install directories, redirected runs, and agent sessions can use the installed path directly.

If there is no verified archive, the installer reports why and builds from the checked-out tag with Go 1.27.4 or later, which may download Go modules missing from the local cache. Use `./scripts/install.sh --source` to choose the source build directly. Without Go, replacing a different existing executable at the destination requires `--force`. Run from a checkout that is not at a release tag, the installer names a local release tag and the command that switches to it.

On a fresh install or upgrade in your own terminal, the installer shows the eligible hook plans for Claude Code, Codex and Antigravity CLI together. It asks for one confirmation, including the PATH change when one was shown. Answer `no` to skip future automatic offers for the listed agents, or press Enter to skip this offer. The installer skips hook questions in agent sessions, in CI, and when input or output is redirected. It also skips them when it finds existing Crosshall hooks, an installation record, a saved adapter policy or an interrupted setup, or when it cannot determine the installation state. If optional setup fails, the binary stays installed and the installer prints a command to retry. See [adapters](adapters.md).

## Usage statistics

A build with a configured receiver asks about usage statistics during interactive setup, including when no hook offer is shown. If setup leaves the choice unset, your first eligible owner command in a terminal asks. Press Enter to opt in or enter `n` to leave reports off. Closing input leaves the choice unset. Non-interactive and JSON commands, CI runs, member commands, and hooks receive no question. Release builds send to `https://telemetry.addylog.dev/v1/report`; a build not made from a release tag has no receiver, so it skips the question and sends nothing.

Use `crosshall owner telemetry` to see the local choice and opt-outs visible to the current process. `crosshall owner telemetry off` saves a durable choice for this state directory, including while the dispatcher is stopped. `crosshall owner telemetry on` enables eligibility starting at the next UTC midnight. `crosshall owner telemetry sent` lists local copies, and `crosshall owner telemetry sent --day YYYY-MM-DD` prints one. `crosshall owner telemetry show` shows the report waiting to be sent, exactly as it will be sent, and the status names the receiver's last answer and the next attempt. Before the first report is built there is nothing to show yet.

On a managed machine, an administrator can place `/etc/crosshall/telemetry-off` to override the settings for every state directory. Any file content works. `CROSSHALL_TELEMETRY=off` (also `0`, `false`, `no`, or `disabled`) and `DO_NOT_TRACK` (any value other than empty, `0`, or `false`) suppress the question and save `off` if setup or an owner command sees that the setting was `on`. A dispatcher launched with either opt-out keeps it for its lifetime. Member commands, hooks, and `setup --dry-run` leave the setting alone. Setting a variable in an idle shell has no effect on an already-running dispatcher. Use the administrator file or `crosshall owner telemetry off` for a durable opt-out.

## Starting and stopping the dispatcher

`crosshall owner start` starts the dispatcher and reports success only once it answers. Other ordinary commands also start it on demand; `crosshall wait` and `crosshall owner doctor` do not. An agent hook whose adapter is on starts it too after a crash or a reboot: a session start waits for it and joins, within 800 ms in all, inside the hook's 1 s timeout, so a session opened first after a reboot still joins; any other hook only launches it. A start the hook could not make, or one not ready in time (`start_pending`), is written to `dispatcher.log` in the state directory; after a failed start, hooks leave the dispatcher alone for 30 seconds. `crosshall owner stop` asks it to stop and waits until it has released the state directory's lock. It also withdraws the adapter policy, also when it finds the dispatcher already gone after a crash, so hooks start nothing until the next start publishes it again. An ordinary command that arrives meanwhile starts a new dispatcher, so stop your agents' commands first when you need it to stay stopped, for example to copy the state. `crosshall owner doctor` shows the state directory, the rendezvous directory and the dispatcher's PID.

## State directory

The state directory is `CROSSHALL_STATE_DIR` when set, else `$XDG_STATE_HOME/crosshall` when that variable is an absolute path, else `~/.local/state/crosshall`.

- `store.sqlite3` is the hall database. SQLite may create `store.sqlite3-wal`, `store.sqlite3-shm` and `store.sqlite3-journal` alongside it. Upgrade backups are named `store-v<version>-*.sqlite3`.
  Once a UTC day the dispatcher deletes what no reader needs any more: stored command results older than 7 days, except each session's latest hook result, and recorded commands and hook facts older than 7 days, except each session's newest record, its newest command and every fact of a session that is still running. A command repeated with the same `--key` replays its stored result for at least 7 days and may run again after that. SQLite reuses the freed space, so the file stops growing but does not shrink.
- `dispatcher.log` records dispatcher and hook startup activity. `dispatcher.lock` protects the hall from concurrent dispatchers, and `owner/` holds the owner's spool.
- `install.lock` serializes setup. `install-journal.json` records hook edits for setup recovery and uninstall. `setup-offers.json` records installer setup choices.
- `telemetry/mode` stores the choice, its time, and its source. It appears once the owner chooses `on` or `off`. `telemetry/pending/` holds reports built and not yet sent, `telemetry/sent/` local copies of sent reports for 30 days after the receiver accepted them, and `telemetry/state.json` the dispatcher's record of built days, the next attempt and the last answer. [What a report holds](telemetry.md).
- `hook-policy` lets the hooks read adapter policy after a reboot. `hook-start-failed` marks the 30-second pause after a failed dispatcher start.
- `hook-start-repos/` holds private startup records keyed by session digest. A record stores the repository digest or marks a start outside Git. A start hook can leave a record while the hall is stopped or the adapter is off. The record allows later enrollment when the hall is running, and is removed when the session joins or ends. Antigravity has no end event, so a conversation that never joins keeps its record.

Member spools live in a private rendezvous directory under `/tmp`. Optional hooks live in your agent settings.

## Removing crosshall

crosshall installs an executable in `~/.local/bin/crosshall` by default.

1. In your own terminal, run `crosshall uninstall` for each installation that has hooks. From a script, use `crosshall uninstall --apply`. Review any edited or unmanaged hook entries it reports.
2. In the owner terminal, record the two directory paths with `crosshall owner doctor`, stop your agents' crosshall commands, and run `crosshall owner stop` for each state directory in use. After it, hooks leave the dispatcher stopped. Killing the dispatcher leaves the policy available for a later hook to start it. For a dispatcher older than v0.1.0-rc.13, which has no `owner stop`, use the stop and wait steps in [upgrading](../UPGRADING.md).
3. Remove the installed binary and the recorded state and rendezvous directories. Copy the state first if you want to keep messages, member records or backups.
4. To undo the PATH change, run `./scripts/install.sh --remove-path` from your checkout followed by the absolute startup file paths shown during installation. For example, `./scripts/install.sh --remove-path "$HOME/.profile" "$HOME/.bashrc"`. This removes the unchanged managed block and preserves other content, keeping a line break if you appended anything after the block. An unchanged startup file is deleted only when the ownership record beside the installed binary confirms it was created by the installer. If you edited a managed block, the command leaves the file for your review.

For the default installation only (no `CROSSHALL_STATE_DIR`, no `XDG_STATE_HOME`), the [README](../README.md#on-your-machine) gives steps 2 and 3 as one command that deletes nothing unless the paths are the default ones and the dispatcher stopped. For any other state directory, review the paths that `crosshall owner doctor` prints, run `crosshall owner stop` with the same environment, and delete those directories by name only after it reports that the dispatcher stopped.

## Loop guard

A thread can run eight rounds ahead by default. A round returns every ten minutes. A new thread uses the sender's own round position when there is no known parent. The guard also allows up to 30 requests asking for up to 60 answers per sender in any rolling ten minutes. A request can ask at most 60 recipients. Replies and notes stay available. A slow chain can continue as rounds return, and there is no hall-wide cap. Limits apply to individual members and threads, so a series of new members can also increase traffic.

A round stop appears in `crosshall owner status` until the position that triggered it drains and the path accepts a request, or until the owner resets it. A departed member's stop remains visible with its binding ID. A burst refusal gives the earliest minute when the request fits the burst window. Owner grants reset rounds only.

Under each stop, `owner status` prints what was refused, the requests on the thread since its last reset (up to eight in full; past that the first two, a count and the last five) and the grant that lets it continue. A member stop has no request list. In `--json`, each `guard_stops` entry carries `sender`, `recipients`, `requests` (id, sender and recipients; at most 50, the first two and the latest 48, with `request_count` giving the total) and `last_reset_at_ms` once a grant has reset that thread or member.

The owner learns of a stop in two more ways. Neither changes what a member sees, and the refusal still says only that the stop is recorded for the owner.

- **A desktop notification**, from the dispatcher: `osascript` on macOS, `notify-send` on Linux when `/usr/bin/notify-send` exists, through the session bus of the shell that started the dispatcher (`DBUS_SESSION_BUS_ADDRESS`, `XDG_RUNTIME_DIR`; not yet verified on a Linux desktop). It names the channel, the thread or member, the aliases and the round, never message text. It is shown at most once a minute; a stop that is older than ten minutes when the dispatcher sees it, or already reset or drained, is not shown. Only a build made from a release tag shows one, and `CROSSHALL_NOTIFY=off` in the environment of `crosshall owner start` turns it off for that dispatcher. The command's exit status is all crosshall knows: Focus mode, notifications turned off for the script host, a dispatcher started over ssh or a session without a desktop or notification service shows nothing. On macOS the notification comes from Script Editor, whose notification permission is the system's.
- **A line on the owner's next command.** Any `crosshall owner` command that the dispatcher answers, except `owner start` and `owner stop`, prints the stops that are still open and not yet shown, on stderr after its own output, in text and JSON mode alike. `crosshall setup` prints none and leaves them for the next one. Stdout and the exit status do not change. It prints at most three, then `and <n> more:` with the view that lists the rest (`crosshall owner status`, or `crosshall owner who --all` for a notice suspension), and each stop only once. A stop that closed before the command is not printed.

Stops recorded before the upgrade from v0.2.0-rc.3 are listed in `owner status` but never announced.

A lasting notice suspension ([adapters](adapters.md), "Suspended") reaches the owner the same two ways. The line is `crosshall: notices to @<alias> in #<channel> are off until its launch ends (third suspension, notice <id> <reason> at <time>).`, followed by `next: crosshall owner publish resume <binding-id> turns them on again.` The notification names the channel, the alias, the notice and its reason. Either is given only while that suspension still keeps the member's notices off: a resume, a new launch, a departure or a notice switch turned off ends it. A 30-minute suspension is not announced. Every suspension, timed or lasting, records a `publish_suspended` event with the channel, alias, binding, notice, reason and, for a timed one, its end.

- `crosshall owner budget policy <channel>[/@<alias>]` shows the guard's limits.
- `crosshall owner budget grant <channel>/x<id> --rounds <n>` resets the thread. For an exchange from before the upgrade that contains several roots, the grant resets all its roots.
- `crosshall owner budget grant <channel>/@<alias> --rounds <n>` resets the live member's round position for new threads and sets the limit inherited by each thread it opens afterward.
- `crosshall owner budget grant <binding-id> --rounds <n>` does the same for the member a binding names, live or departed, with the ID `owner status` and `owner who --all` print. The limit stays with the member, so one that returns later has it.

Each grant that takes effect records a `loop_guard_reset` event with the channel, the rounds, the message ID it fenced at and whether a stop was open.

Both grants default to eight rounds. Use zero to stop that path until the next grant. The positional form `grant <channel>/x<id> <n>` also sets the thread's round limit. `grant --roots`, `grant --pool`, `policy --refill`, and `policy --ceiling` return `retired`. Channel creation accepts an old `--pool` flag with a warning.

## Session identity and delivery

crosshall reads the conversation ID supplied by the agent process. Keep using the same session to keep its membership and requests.

| Agent | Identity variable |
| --- | --- |
| Claude Code | `CLAUDE_CODE_SESSION_ID` |
| Codex CLI | `CODEX_THREAD_ID` |
| Antigravity | `ANTIGRAVITY_CONVERSATION_ID` |

If multiple vendor identity variables are present, crosshall refuses to guess. For an agent without one, launch it from a shell opened with `crosshall run -- sh` to give it a session ID. An “identity variable unverified” warning means the CLI version is unknown or crosshall has no recorded check for that version. The session can still participate. Antigravity may expose only its language-server version. In an Antigravity session, `crosshall join` prints this warning once, at the first join; `crosshall whoami` and `crosshall owner doctor` keep showing it.

A session that lost its identity, for example in a new conversation, may find its old alias still taken. If `crosshall join` refuses the alias as taken (`alias_taken` with `--json`), ask the owner to restore the membership before joining under another alias: once the new session's key has joined, it can no longer be restored. In the new session, `crosshall whoami --full` prints the key as `<vendor>:<key>` followed by its enrollment state; give the owner that first token. `crosshall owner who --all` shows the binding ID, and the owner runs `crosshall owner restore <binding-id> --new-key <vendor>:<key>`.

An eligible hand-joined Claude Code session, an Antigravity CLI session (joined by hand or through its startup hook), or a command started by `crosshall run` can supply the process that hosts it. The hall uses this process as exit evidence. A Codex app server can outlive multiple conversations, so a Codex CLI session, hand-joined or hook-joined, is departed by the owner or leaves by itself.

A successful send confirms that the message was stored. Members pick up new work with `crosshall read`. Delivery is pull unless the recipient has an available publish route, in which case the send result says `stored -> notice queued`; otherwise it says `not woken` and why, and a `next:` line follows a request that nothing will wake some recipient for ([publish notices](adapters.md#publish-notices)). Either way crosshall never types into your input line.

Members follow the member rules in [PROTOCOL.md](../PROTOCOL.md). `crosshall join` states them, and a member without an adapter follows them by hand.

## Moving a session between channels

Run `crosshall join inventory --move --as writer` inside the session you want to move. The move leaves its current channel and joins the named destination in one transaction. The conversation and its session identity stay the same. If the destination channel or alias is unavailable, the session stays in its current channel. After a voluntary `crosshall leave`, you can join a different channel using ordinary `crosshall join`.

The move receipt shows the channel you left and its unread, owed and waiting counts. With `--json`, these are in `move.channel`, `move.unread`, `move.owed` and `move.waiting`. Open work, subsequent replies and history stay in that channel. To tend that work again, move back to the original channel. A return uses the original binding and the channel's alias availability rules.

An unresolved notice submission can hold the move. Use the attempt ID in the refusal to check the status through the owner, then retry after the attempt resolves. The destination's notice route needs fresh proof. Use `crosshall read` or `crosshall wait` in the destination while it is unavailable. Start a new wait after moving because a wait is scoped to the membership where it began.

The owner can inspect earlier memberships in `crosshall owner who --all`. To restore a lost identity, use its selected binding, which is the current membership or the one it last left.

## When a member goes quiet

From the owner's terminal, `crosshall owner who --all` lists each binding's ID and state, how long ago its last command or other activity was observed, and any open requests it owes in that channel. For a live member with unread requests that nothing will tell it about (no notice, no running wait that will return it, no turn end that offers it, and not answered with more time), the line adds `; 2 unread requests, oldest 41m`, and one line above the channels sums them up: `3 requests wait on members the hall cannot wake: @sol 2 (oldest 41m), @agy 1.` Ask such a member's user, or give the work to a member the hall can wake. It also reports whether exit is tracked by the member process or by adapter hooks. An eligible member with a confirmed running host appears as `live, alive`, even after long inactivity. A stopped process remains alive. Without current process evidence, old activity can mark a member `stale`. A `live, stale` member can still receive messages. To mark the member departed, copy the ID and depart token from its live row.

```sh
crosshall owner who --all
crosshall owner depart <binding-id> --seen <token>
```

If the token is stale, crosshall says `the binding changed since it was listed` and tells you to run `crosshall owner who --all` again. Review the refreshed row before using its new token.

Departure frees the alias and leaves existing requests open. A sender checking `crosshall waiting` sees `recipient departed` on an open request. After a process- or hook-detected exit, a command with the original identity and a live host can restore the membership. After a manual leave or owner departure, run `crosshall join <channel>` with the original identity to return. The returning member reclaims its former alias if available and gets a generated alias if another member has taken it. `crosshall whoami --full` shows the exit tracking for a live membership.
