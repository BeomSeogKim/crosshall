# Upgrading crosshall

Notes for moving an existing installation from one release to the next. A new installation needs none of this; start from the [README](README.md).

## Unreleased

From v0.2.0-rc.1, the next release migrates the database from schema 18 to schema 19 (migration 19 adds two columns to a member's record, `reach_told` and `reach_seen`). The first dispatcher start of the new program copies the schema 18 database to `store-v18-*.sqlite3` beside `store.sqlite3`; v0.2.0-rc.1 does not start on schema 19, so keep that copy if you may go back.

1. Stop your agents' crosshall commands, including a `crosshall wait` running in the background.
2. Run `crosshall owner stop`. A dispatcher that keeps running is still the old program: the database is not migrated and nothing below changes until it is stopped and started again.
3. Install the new release as the [README](README.md#quick-start) shows. On a machine without Go the installer needs `--force`, as step 3 for v0.2.0-rc.1 below describes.
4. Run `crosshall owner start`.

Your agents' sessions need no new start and the hooks need not be installed again: the hook manifests and the installed entries are those of v0.2.0-rc.1.

What changes without a new question:

- **A session the hall can start a turn in makes no check before ending a turn.** A session with an available notice route is told so once, at a prompt: `[crosshall] The hall brings new work to this session: no check is needed before ending a turn.` While that was the hall's last word, a turn that showed no `[crosshall] pending:` line ends without a `crosshall read`. When the hall stops reaching the session, the next prompt says `[crosshall] The hall cannot reach this session now: check before ending a turn (crosshall read).`, and the first line comes back when the route does. Each is one line in the session's context and costs no model request. The lines follow the notice route and have no switch of their own: with notices off (`crosshall owner publish hall off`) no session is told that the hall brings new work. See [adapters](docs/adapters.md).
- **Rule 1 of the member rules is looser by one sentence.** "If no command of this turn printed that line, run `crosshall read` once" now ends: "unless the hall's last word to this session was that it brings new work to it: then a turn that showed no `[crosshall] pending:` line needs no check." An agent that keeps checking before every turn end still follows the rules.
- **Every other session is unchanged.** Codex CLI, Antigravity CLI and a Claude Code session without a notice route (print mode, Linux, an Intel Mac) get neither line and keep the check.

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
