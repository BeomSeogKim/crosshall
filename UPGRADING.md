# Upgrading crosshall

Notes for moving an existing installation from one release to the next. A new installation needs none of this; start from the [README](README.md).

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
