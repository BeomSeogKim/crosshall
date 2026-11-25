# Upgrading crosshall

Notes for moving an existing installation from one release to the next. A new installation needs none of this; start from the [README](README.md).

## Unreleased

Upgrading from v0.3.0 moves the database from schema 29 to schema 30. Migration 030 adds two columns to `adapter_launch` (`reconciled_seq`, `reconciled_at_ms`) and the index `turn_placeholder_finishing`; on a copy of a running hall's store (13,854 observations, 1,704 turns) it took 20 ms. A launch from before the upgrade has no record, so its next fold reads all its facts once, as v0.3.0 does.

1. Stop running crosshall commands, including background waits, and run `crosshall owner stop`.
2. Copy `store.sqlite3` from the stopped hall's state directory to a private backup. On its first start, the new dispatcher also saves a `store-v29-*.sqlite3` backup beside the database.
3. Install the new release at the path your members and installed hooks use, then run `crosshall owner start` and `crosshall owner doctor`. No setup is needed.

To roll back, stop the dispatcher, restore the schema 29 database from your copy (or the `store-v29-*.sqlite3` backup) and start v0.3.0, which refuses a schema 30 store. Restoring discards messages and other changes made since the upgrade.

## To v0.3.0

Upgrading from v0.3.0-rc.2 keeps the database at schema 29. No migration runs, and no setup is needed.

1. Stop running crosshall commands, including background waits, and run `crosshall owner stop`.
2. Keep a private copy of the stopped hall's `store.sqlite3`. Install v0.3.0 at the path your members and installed hooks use (the [quick start](README.md#quick-start) has the command).
3. Run `crosshall owner start`, then `crosshall owner doctor`.

To go back to v0.3.0-rc.2, stop the dispatcher and running commands, put the rc.2 binary back at the same path and start it on the same schema 29 store.

Coming from v0.2.0, follow [To v0.3.0-rc.1](#to-v030-rc1) with the v0.3.0 binary: it adds `crosshall setup antigravity --apply` and the Codex managed app server step, and its notes on going back to v0.2.0 apply unchanged.

## To v0.3.0-rc.2

Upgrading from v0.3.0-rc.1 keeps the database at schema 29. No migration runs, and no setup is needed.

1. Stop running crosshall commands, including background waits, and run `crosshall owner stop`.
2. Keep a private copy of the stopped hall's `store.sqlite3`. Install v0.3.0-rc.2 at the path your members and installed hooks use (the [quick start](README.md#quick-start) has the command).
3. Run `crosshall owner start`, then `crosshall owner doctor`.

The first start recreates any member spool directory that macOS's daily `/tmp` cleaner removed, with one log line for each. To go back to v0.3.0-rc.1, stop the dispatcher and running commands, put the rc.1 binary back at the same path and start it on the same store. Coming from v0.2.0, follow the section below as well.

## To v0.3.0-rc.1

Upgrading from v0.2.0 keeps the database at schema 29. No migration runs.

1. Stop running crosshall commands, including background waits, and run `crosshall owner stop`.
2. Keep a private copy of the stopped hall's `store.sqlite3`. Install v0.3.0-rc.1 at the path your members and installed hooks use (the [quick start](README.md#quick-start) has the command).
3. Run `crosshall owner start`, then `crosshall owner doctor`.
4. If you use Antigravity CLI, run `crosshall setup antigravity --apply` to move its hooks to manifest `agy-2` (below).
5. For Codex notices, run Codex sessions in Codex's managed app server (the default) with crosshall's hooks trusted (`/hooks` in Codex, then reopen it). A session started with "Run without daemon this time" is not woken; it sees new work at its next prompt.

To go back to v0.2.0, stop the dispatcher and running commands, put the v0.2.0 binary back at the same path and start it on the same schema 29 store; it ignores the new indexes. With Antigravity hooks installed, follow the steps below first.

Antigravity hooks move to manifest `agy-2`: `crosshall setup antigravity` replaces the `agy-1` group (SessionStart only) with one that also holds `PreInvocation` and `Stop`. Setup changes only its own group; when you have not edited the file since setup, it starts again from your original bytes, which uninstall still restores. Until you run it, `crosshall owner doctor` and `crosshall setup status` name the step.

To go back to v0.2.0 or earlier with Antigravity hooks installed, remove them with this release first, because v0.2.x's setup reads only a SessionStart-only group:

1. Run `crosshall uninstall antigravity --apply` with this release.
2. Put the older binary back at the same path, then run `crosshall setup antigravity --apply` to install `agy-1`.

If you already switched back, v0.2.x shows the Antigravity hooks as `unknown` (`invalid setup entry`) and its setup refuses them. Its `uninstall antigravity --apply` restores your original file only when you have not edited it since setup; after an edit it reports `removed` but leaves the crosshall group. In that case put this release back, run `crosshall setup antigravity --apply` and then `crosshall uninstall antigravity --apply`, and switch again. Meanwhile an older hook answers the new callbacks with `{}`, and Antigravity carries on.

The first start of this dispatcher adds three indexes to `store.sqlite3` (`observation_launch_hook`, `observation_launch_turn`, `turn_launch_opened`); the schema version stays 29. On a store of about 14,000 observations this took 26 ms, and on a million observations 2.4 s; on a store that large, a session that starts while the dispatcher builds them may miss its start's wait, and its next prompt recovers it. Going back to v0.2.0 needs no step: v0.2.0 opens the store, ignores the indexes, and SQLite keeps them current.

Known issue when going back to v0.2.0 or earlier: a hook of the older binary does not recognize a resume notice (`... The hall restarted and you still owe these requests ...`) that the session echoes, so its receipt never comes. A resume still being submitted when you switch back then holds that session's notices until that session ends or is restarted; v0.2.0 has no command that releases it.

## To v0.2.0

Upgrading from v0.2.0-rc.5 keeps the database at schema 29. No migration runs.

1. Stop running crosshall commands, including background waits, and run `crosshall owner stop`.
2. Keep a private copy of the stopped hall's `store.sqlite3`. Install v0.2.0 at the path your members and installed hooks use (the [quick start](README.md#quick-start) has the command).
3. Run `crosshall owner start`, then `crosshall owner doctor`.

When setup's hook entries are still on disk, their recorded revision differs from the one this binary installs and a hook of the current revision is missing, `crosshall owner start`, `crosshall owner doctor` and `crosshall setup status` now say so and name the setup command, for example `The Claude Code hooks on disk were installed for claude-2; this crosshall installs claude-3. Run crosshall setup claude.` Run the command it names. The adapter object in `owner doctor --json` and `setup status --json` gains `installed_rev`, which can name an older revision even when no hook is missing and no sentence is shown.

To go back to v0.2.0-rc.5, stop the dispatcher and running commands, put the rc.5 binary back at the same path and start it on the same schema 29 store. Coming from v0.2.0-rc.4 or earlier, follow the sections below as well.

## To v0.2.0-rc.5

Upgrading from v0.2.0-rc.4 to v0.2.0-rc.5 moves the database from schema 28 to schema 29. Migration 029 rebuilds the `observation` table to admit Claude Code's idle report; its rows and indexes are kept.

1. Stop running crosshall commands, including background waits, and run `crosshall owner stop`.
2. Copy `store.sqlite3` from the stopped hall's state directory to a private backup. On its first start, the v0.2.0-rc.5 dispatcher also saves a `store-v28-*.sqlite3` backup beside the database.
3. Install rc.5 at the path your members and installed hooks use (the [quick start](README.md#quick-start) has the command), then run `crosshall owner start` and `crosshall owner doctor`.
4. Run `crosshall setup claude` to move the Claude Code hooks to manifest `claude-3`, whose Notification hook also matches `idle_prompt`. Setup replaces only its own notification hook, and on uninstall still restores the original file when you have not edited it since. Sessions started before keep the `claude-2` hooks until they start again.

Sessions keep their membership. To roll back, stop the rc.5 dispatcher, restore the schema 28 database from your pre-upgrade copy (or the `store-v28-*.sqlite3` backup) and start the rc.4 binary. Restoring discards messages and other changes made since the upgrade. Coming from v0.2.0-rc.3 or earlier, follow the sections below as well.

When a dispatcher holds its lock, `owner start` now waits up to five seconds from the command's start for it to confirm readiness; with no lock holder it still gives up after about three seconds. `setup --apply` and `uninstall --apply` each stop after 30 seconds; after a timeout, run `crosshall setup status` and retry with the same adapters.

Rule 4 and the notice text change:

- A session that joined before the upgrade keeps the old text of rule 4 until it runs `crosshall join <channel> --rules`.
- A notice now ends `Run crosshall read, then use crosshall reply to answer or decline each request it shows, as the member rules say.` A notice sent with the v0.2.0-rc.4 text before the upgrade still counts as a notice when the session receives it.
- For a hook-joined Codex session whose next prompt shows new work, an empty `crosshall inbox` prints `Inbox is empty. Do not wait: new work shows at your next prompt. End the turn when you have nothing else to do.`, and a wait that times out with nothing unread names that step instead of the wait command. `crosshall inbox --json` and an arriving wait's JSON gain `at_next_prompt`. Other sessions see no change.

In text output, the sender hint now begins `next: nothing will wake`; it said `next: nothing will tell`. When a running turn defers a notice and no wait accepts the work, the send delivery and the corresponding waiting recipient can report `why=turn_open`. Both omit `seen_at` unless the turn-end check offers the request. The send delivery also sets `next` to `at its turn-end check` or `the hall checks again after its turn`.

A script that reads a send result's, `who`'s or `waiting`'s `why` gets `no_route` instead of `off` for a member whose notice route the hall invalidated, such as a Claude session whose socket no hook has reported yet. `off` now means only a switch, do not disturb, or a route that ended.

## To v0.2.0-rc.4

Upgrading from v0.2.0-rc.3 to v0.2.0-rc.4 moves the database from schema 22 to schema 28. The migrations run in this order:

- Migration 023 records whether a session received the owner resume command for a lasting suspension.
- Migration 024 adds the `owner_alert` table, which records how each new round stop reached the owner. Stops recorded before the upgrade get no row: they stay in `crosshall owner status` and are never announced.
- Migration 025 records the server process and home a Codex daemon start names on the session's route, and gives each open notice a lane: notices recorded before it keep their process's lane, so one notice at a time stays open per process as before. A Codex route registered before it names no server and reads `codex daemon not confirmed` until the session's next start.
- Migration 026 records why a notice receipt was `unknown`. Receipts observed before it have no cause; `owner doctor` counts them as `cause not recorded`.
- Migration 027 records when the hook confirms writing a pending line. Earlier offers have no write confirmation. Still-unread IDs can appear again at the next eligible prompt. An older hook continues to count its offers when the dispatcher prepares them.
- Migration 028 records the selected channel binding for each enrolled session and enforces one live binding per session. It uses existing membership records, including departed bindings, and preserves the message and request ledger. If existing records leave the selected binding ambiguous, the migration stops and reports the affected IDs.

1. Stop your agents' crosshall commands, including any background `crosshall wait`.
2. Run `crosshall owner stop`.
3. Copy `store.sqlite3` from the stopped hall's state directory to a private backup. On its first start, the v0.2.0-rc.4 dispatcher also saves a `store-v22-*.sqlite3` backup beside the database.
4. Install v0.2.0-rc.4 using the [README](README.md#quick-start) clone command. On a machine without Go, see the installer instructions in step 3 of the v0.2.0-rc.1 upgrade below.
5. Run `crosshall owner start`. Set `CROSSHALL_NOTIFY=off` in its environment if you want no desktop notifications.

To roll back, stop the v0.2.0-rc.4 dispatcher, restore the schema 22 database from your pre-upgrade copy and restart the v0.2.0-rc.3 binary. Restoring the copy discards messages and other changes made since the upgrade.

If migration 028 stops during an upgrade from v0.2.0-rc.3, the dispatcher stays down and the database remains at schema 27. The v0.2.0-rc.3 dispatcher requires schema 22. While the dispatcher is stopped, set aside `store.sqlite3` and any `-wal` or `-shm` files beside it. Copy the `store-v22-*.sqlite3` backup from that upgrade to `store.sqlite3` and start the v0.2.0-rc.3 binary.

Use the upgraded binary for member commands and hooks after restarting the dispatcher. If you were already running a development build, the automatic backup matches the schema of that build, for example `store-v27-*.sqlite3` when upgrading from schema 27.

Sessions need no new start, and the hooks need not be installed again. A session that joined before the upgrade keeps the old text of rule 4 until it runs `crosshall join <channel> --rules`. The usage statistics question is not asked again: your answer stays, and reports now use registry 2, which adds the `send.wake` counts. See [telemetry](docs/telemetry.md).

After its first start, and then once a UTC day, the v0.2.0-rc.4 dispatcher deletes stored command results, recorded commands and hook facts that are older than 7 days. [Operations](docs/operations.md#state-directory) lists what it keeps at any age. Your pre-upgrade copy still holds them. A command repeated with the same `--key` replays its stored result for at least 7 days and may run again after that.

Some output that a script may read has changed. For each recipient, a send result now says `stored -> notice queued`, `stored -> its wait is running` or `stored -> not woken: <reason>` and where the recipient sees the message; it used to say `notice queued` or `waits for its next crosshall command`. `crosshall who` ends a member's line with `woken by a notice`, `woken by its wait` or `not woken: <reason>`, where it said `pull` or the route's state. In JSON nothing is removed: each delivery of a send result gains `wake`, `why`, `seen_at` and `until_ms`, and its `next` can be `its wait is running`. `crosshall wait` can end with exit code 1 and the error code `wait_host_gone` when the agent process it names has ended. A failed `crosshall owner start` now begins `owner start failed:`. `crosshall owner who` can end a member's line with its unread requests that nothing will tell it about, for example `; 2 unread requests, oldest 41m`, and print one line above the channels that names those members; in its JSON such a member carries `unread_not_woken`. In `owner doctor` JSON, the counters gain `unknown_causes`.

An unknown notice receipt suspends its session's notices for 30 minutes. If an older dispatcher made an unknown suspension last until the session ended, the v0.2.0-rc.4 dispatcher treats it as expiring 30 minutes after it was recorded. A session that already received the old reach-loss line gets the owner resume command at its next prompt while the suspension is in effect.

A new round stop shows a desktop notification where the system can show one, and appears once, on stderr, on the owner's next command. A third suspension of a launch's notices, which lasts until that launch ends, is announced in the same two ways. See [loop guard](docs/operations.md#loop-guard). In owner status JSON, each `guard_stops` entry gains `sender`, `recipients`, `requests`, `request_count` and, once a grant has reset its thread or member, `last_reset_at_ms`. `owner budget grant <binding-id>` resets a member by binding, including a departed one.

## To v0.2.0-rc.3

Upgrading from v0.2.0-rc.2 to v0.2.0-rc.3 moves the database from schema 21 to schema 22. A hall that ran rc.2 can already have recorded message levels and round positions. Migration 022 clears those positions and fences existing requests and members at the highest message ID. The round guard starts each path at zero, with the existing messages, deadlines, obligations and operation keys available.

1. Stop your agents' crosshall commands, including any background `crosshall wait`.
2. Run `crosshall owner stop`.
3. Copy `store.sqlite3` from the stopped hall's state directory to a private backup. On its first start, the v0.2.0-rc.3 dispatcher also saves a `store-v21-*.sqlite3` backup beside the database.
4. Install v0.2.0-rc.3 using the [README](README.md#quick-start) clone command. On a machine without Go, see the installer instructions in step 3 of the v0.2.0-rc.1 upgrade below.
5. Run `crosshall owner start`.

To roll back, stop the v0.2.0-rc.3 dispatcher, restore the schema 21 database from your pre-upgrade copy and restart the v0.2.0-rc.2 binary. Restoring the copy discards messages and other changes made since the upgrade.

The loop guard replaces exchange units, root tokens and the join pool. Each independent request opens its own exchange. Rule 6 tells members to reply or note what they have when a request stops. Owner status shows active stops, and `owner budget grant <channel>/x<id> --rounds <n>` and `owner budget grant <channel>/@<alias> --rounds <n>` reset the corresponding scope. A positional number on the thread form sets rounds. `grant --roots`, `grant --pool`, `policy --refill` and `policy --ceiling` exit 3 with `retired`. `owner channel create --pool` still creates the channel, prints a warning and returns `pool_ignored: true` in JSON.

In `ask --json`, `round` replaces `debit` and `remaining`. Exchange fields stay. In member and owner status JSON, `round`, `round_limit` and `guard_stops` replace `granted`, `spent`, `remaining`, `root_tokens`, `next_root_token_at_ms`, `join_pool` and `budget_exhaustions`. Replaying a previously stored mutation key still returns its committed result.

## To v0.2.0-rc.2

From v0.2.0-rc.1, this release migrates the database from schema 18 to schema 21. Migration 19 records the reachability advice a member received. Migration 20 adds `member_host` to record the process hosting a member. Migration 21 adds the records of budget rounds; this release only records them and changes no behavior. The first v0.2.0-rc.2 dispatcher start copies the schema 18 database to `store-v18-*.sqlite3` beside `store.sqlite3`; v0.2.0-rc.1 does not start on schema 21, so keep that copy if you may go back.

1. Stop your agents' crosshall commands, including a `crosshall wait` running in the background.
2. Run `crosshall owner stop`. A dispatcher that keeps running is still the old program: the database is not migrated and nothing below changes until it is stopped and started again.
3. Install v0.2.0-rc.2 as the [README](README.md#quick-start) shows, with `v0.2.0-rc.2` as the tag in the clone command. On a machine without Go the installer needs `--force`, as step 3 for v0.2.0-rc.1 below describes.
4. Run `crosshall owner start`.

Your agents' sessions need no new start and the hooks need not be installed again: the installed hook entries are the same as those of v0.2.0-rc.1.

The usage statistics setting starts unset after the upgrade and is stored in a local file. A terminal setup or the first eligible owner command can ask. Nothing is sent before it is answered, and then only reports about whole UTC days after the answer, by the dispatcher through `/usr/bin/curl`. Reports go to `https://telemetry.addylog.dev/v1/report`; a build not made from a release tag neither asks nor sends. `crosshall owner telemetry show` shows the report waiting to be sent, exactly as it will be sent. `crosshall owner telemetry off` and `/etc/crosshall/telemetry-off` stop it before the next request; `CROSSHALL_TELEMETRY=off` and `DO_NOT_TRACK` stop a dispatcher started with them. See [usage statistics](docs/operations.md#usage-statistics) and [what a report holds](docs/telemetry.md).

What changes without a new question:

- **A session the hall can start a turn in makes no check before ending a turn.** A session with an available notice route is told so once, at a prompt: `[crosshall] The hall brings new work to this session: no check is needed before ending a turn.` While that was the hall's last word, a turn that showed no `[crosshall] pending:` line ends without a `crosshall read`. When the hall stops reaching the session, the next prompt says `[crosshall] The hall cannot reach this session now: check before ending a turn (crosshall read).`, and the first line comes back when the route does. Each is one line in the session's context and costs no model request. The lines follow the notice route and have no switch of their own. A session whose effective notice setting is off is not told that the hall brings new work. See [adapters](docs/adapters.md).
- **Rule 1 of the member rules is looser by one sentence.** "If no command of this turn printed that line, run `crosshall read` once" now ends: "unless the hall's last word to this session was that it brings new work to it: then a turn that showed no `[crosshall] pending:` line needs no check." An agent that keeps checking before every turn end still follows the rules.
- **Other sessions keep the end-of-turn check.** Codex CLI, Antigravity CLI and Claude Code sessions without a notice route still run the check.
- **A member departs when its process exits.** For a Claude Code, Antigravity CLI or `crosshall run` member, the hall records the process its commands run in when the command can name that process and, once that process is proven gone, departs the member and keeps its open requests. The member's next command other than `wait` from a running process brings it back, under its old alias when that is free. A Codex CLI member's exit is not tracked: it stays until it leaves or the owner departs it.
- **Tracking starts with the next eligible member command.** Existing memberships and open requests remain in place. A reportable host can be learned without a fresh join. See [when a member goes quiet](docs/operations.md#when-a-member-goes-quiet).

## To v0.2.0-rc.1

From v0.1.0, this release migrates the database from schema 6 to schema 18 (migrations 7 to 18). The first v0.2.0-rc.1 dispatcher start copies the schema 6 database to `store-v6-*.sqlite3` beside `store.sqlite3`; v0.1.0 does not start on schema 18, so keep that copy if you may go back.

1. Stop your agents' crosshall commands, including a `crosshall wait` running in the background.
2. Run `crosshall owner stop`. A dispatcher that keeps running is still the old program: on Linux a `crosshall join` typed inside a Claude Code session is then refused with `bad arguments` until the dispatcher is stopped and started again.
3. Install v0.2.0-rc.1 as the [README](README.md#quick-start) shows. On a machine without Go the installer cannot check the program already at `~/.local/bin/crosshall` and stops with `Could not recognize the existing executable at <path> without Go. Use --force to replace it.` Confirm that the file at that path is your crosshall binary (`~/.local/bin/crosshall version` prints its version), then run `./scripts/install.sh --force`.
4. Run `crosshall owner start`.
5. Start your Claude Code sessions again, or resume them. A session that kept running is still recorded under the old hook manifest (`claude-1`); notices and the turn-end check need a session start under this release (`claude-2`).

What changes without a new question, if you had installed agent hooks before:

- **Notices and the turn-end check are on** for Claude Code on macOS arm64 once its adapter is enabled: the hall starts a turn in an idle interactive session when work arrives for it (one model turn for each notice), and when a turn ends while a request waits that is unread, unanswered and not yet announced to the session, the turn continues once (one model request). `crosshall owner publish hall off` stops notices, and `crosshall owner adapter set claude --turn-end off` stops the turn-end check.
- **Automatic joining stays as it was.** An upgrade alone does not turn it on. Installing an agent's hooks again turns it on unless you already ran `crosshall owner channel auto on` or `off`; the install question says so.
- **A hook-joined session starts with one line** instead of the full member rules, and gets the rules with its first crosshall command.
- **Member commands no longer end with `Run crosshall inbox`.** Every member command ends with `[crosshall] N unread, M owed.`, and `crosshall read` with no argument picks up what is unread.

## To v0.1.0

From rc.13, v0.1.0 keeps schema 6, so there is no migration. Stop your agents' crosshall commands, run `crosshall owner stop` (agent hooks of later releases start a killed dispatcher again, but not one stopped this way), copy your crosshall state directory if you want a backup, install v0.1.0 and run `crosshall owner start`. From rc.12 or earlier, follow the rc.13 steps below; they apply to v0.1.0 as well.

## To rc.13

rc.13 migrates the database to schema 6. The running rc.12 dispatcher has no `owner stop`, so stop it the same way as for rc.12, and stop your agents' crosshall commands first, since an ordinary command starts a dispatcher again:
1. `crosshall owner doctor` shows the dispatcher's PID. Stop it with `kill <pid>`.
2. Wait until `crosshall owner doctor` reports that the dispatcher is not running.
3. Copy your crosshall state directory.
4. Install the new release and run `crosshall owner start`.

The first rc.13 dispatcher start copies the schema 4 database to `store-v4-*.sqlite3` beside `store.sqlite3`. rc.12 does not start on schema 6. From rc.13 on, `crosshall owner stop` replaces steps 1 and 2.

What changes:
- **Join pools.** A channel created from now on holds a join pool of root tokens (8 by default, set with `--pool`), and each member's first join takes one. A channel that existed before the upgrade gets a pool of 0: nothing is handed out, and its members keep what you granted. To give an existing channel a pool, run `crosshall owner budget grant <channel> --pool <n>`.
- **Installing.** The installer installs the release's prebuilt binary after checking its checksum, falls back to a source build, and prints what it installed, a PATH line when needed, and the next command. See [operations](docs/operations.md#installing).
- **Linked handoffs.** A member that owes an answer in an exchange with no units left can open a new exchange for a handoff with `crosshall ask @alias "..." --new --re <owed-request-id>`, which spends one of its root tokens.
- **Owner commands.** `crosshall owner stop` stops the dispatcher and waits for it to exit. `crosshall owner doctor` shows the state and rendezvous directories.

## To rc.12

rc.12 migrates the database to schema 4. The installer replaces the binary but does not stop a running dispatcher, so first stop it and back up:
1. `crosshall owner doctor` shows the dispatcher's PID. Stop it with `kill <pid>`.
2. Wait until `crosshall owner doctor` reports that the dispatcher is not running.
3. Copy your crosshall state directory.
4. Install rc.12 and run `crosshall owner start`.

When you upgrade from rc.11, the first rc.12 dispatcher start also copies the schema 3 database to `store-v3-*.sqlite3` beside `store.sqlite3`. The rc.11 dispatcher does not start on schema 4.

What changes for a member:
- The join hint states the six member rules of [PROTOCOL.md](PROTOCOL.md).
- A result points to the inbox only when something is unread or owed.
- A request the owner cancelled before you read it is listed apart, with no answer owed.
- Every suggested wait is `crosshall wait --existing --work-only --timeout 9m`.

## To rc.10

rc.10 uses the same database schema as rc.9. Earlier Codex hooks still get their state directory from the environment Codex runs them in. With a shared daemon, that environment comes from the session that started it.

Run `crosshall setup codex` with the same crosshall state directory you used to install the hooks. Apply the changes, then review and trust the updated commands in Codex. To use a different state directory, use a separate `CODEX_HOME` or first run `crosshall uninstall codex` with the original state directory.

## From rc.8 to rc.9 or later

The first dispatcher start on rc.9 or rc.10 backs up the schema 2 database as `store-v2-*.sqlite3` beside `store.sqlite3`, then migrates it to schema 3. The rc.8 dispatcher refuses schema 3. Rolling back this state requires the schema 2 backup, which contains only data written before the migration.
