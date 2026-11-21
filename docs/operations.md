# Operating crosshall

For the owner: identities, members that go quiet, and the commands that run from your own terminal. The member rules are in [PROTOCOL.md](../PROTOCOL.md).

## Installing

`./scripts/install.sh`, run from a checkout at a release tag, installs `~/.local/bin/crosshall`. When the release has binary assets, the installer downloads the archive for macOS or Linux on arm64 or x86-64 and verifies its SHA-256 checksum before it unpacks the executable. The checksum verifies archive integrity; the builder's identity remains unverified. This path uses curl and either `sha256sum` or `shasum`. GNU tar also needs `gzip` on PATH to read the archive.

For a default install in your own terminal, if the just-installed `crosshall` does not win PATH lookup, the installer shows each startup file and the exact block that places `~/.local/bin` first. A combined offer covers the displayed PATH and hook changes in one confirmation. New Bash and zsh shells, POSIX login shells that read `.profile`, and agent sessions using those shells or their PATH can then find `crosshall`. The current shell can use the absolute path in the install receipt immediately. Custom install directories, redirected runs, and agent sessions can use the installed path directly.

If there is no verified archive, the installer reports why and builds from the checked-out tag with Go 1.27.4 or later, which may download Go modules missing from the local cache. Use `./scripts/install.sh --source` to choose the source build directly. Without Go, replacing a different existing executable at the destination requires `--force`. Run from a checkout that is not at a release tag, the installer names a local release tag and the command that switches to it.

On a fresh install or upgrade in your own terminal, the installer shows the eligible hook plans for Claude Code, Codex and Antigravity CLI together. It asks for one confirmation, including the PATH change when one was shown. Answer `no` to skip future automatic offers for the listed agents, or press Enter to skip this offer. The installer skips hook questions in agent sessions, in CI, and when input or output is redirected. It also skips them when it finds existing Crosshall hooks, an installation record, a saved adapter policy or an interrupted setup, or when it cannot determine the installation state. If optional setup fails, the binary stays installed and the installer prints a command to retry. See [adapters](adapters.md).

## Starting and stopping the dispatcher

`crosshall owner start` starts the dispatcher and reports success only once it answers. Other ordinary commands also start it on demand; `crosshall wait` and `crosshall owner doctor` do not. An agent hook whose adapter is on starts it too after a crash or a reboot: a session start waits for it and joins, within 800 ms in all, inside the hook's 1 s timeout, so a session opened first after a reboot still joins; any other hook only launches it. A start the hook could not make, or one not ready in time (`start_pending`), is written to `dispatcher.log` in the state directory; after a failed start, hooks leave the dispatcher alone for 30 seconds. `crosshall owner stop` asks it to stop and waits until it has released the state directory's lock. It also withdraws the adapter policy, also when it finds the dispatcher already gone after a crash, so hooks start nothing until the next start publishes it again. An ordinary command that arrives meanwhile starts a new dispatcher, so stop your agents' commands first when you need it to stay stopped, for example to copy the state. `crosshall owner doctor` shows the state directory, the rendezvous directory and the dispatcher's PID.

## State directory

The state directory is `CROSSHALL_STATE_DIR` when set, else `$XDG_STATE_HOME/crosshall` when that variable is an absolute path, else `~/.local/state/crosshall`.

- `store.sqlite3` is the hall database. SQLite may create `store.sqlite3-wal`, `store.sqlite3-shm` and `store.sqlite3-journal` alongside it. Upgrade backups are named `store-v<version>-*.sqlite3`.
- `dispatcher.log` records dispatcher and hook startup activity. `dispatcher.lock` protects the hall from concurrent dispatchers, and `owner/` holds the owner's spool.
- `install.lock` serializes setup. `install-journal.json` records hook edits for setup recovery and uninstall. `setup-offers.json` records installer setup choices.
- `hook-policy` lets the hooks read adapter policy after a reboot. `hook-start-failed` marks the 30-second pause after a failed dispatcher start.
- `hook-start-repos/` holds private startup records keyed by session digest. A record stores the repository digest or marks a start outside Git. A start hook can leave a record while the hall is stopped or the adapter is off. The record allows later enrollment when the hall is running, and is removed when the session joins or ends.

Member spools live in a private rendezvous directory under `/tmp`. Optional hooks live in your agent settings.

## Removing crosshall

crosshall installs an executable in `~/.local/bin/crosshall` by default.

1. In your own terminal, run `crosshall uninstall` for each installation that has hooks. From a script, use `crosshall uninstall --apply`. Review any edited or unmanaged hook entries it reports.
2. In the owner terminal, record the two directory paths with `crosshall owner doctor`, stop your agents' crosshall commands, and run `crosshall owner stop` for each state directory in use. After it, hooks leave the dispatcher stopped. Killing the dispatcher leaves the policy available for a later hook to start it. For a dispatcher older than v0.1.0-rc.13, which has no `owner stop`, use the stop and wait steps in [upgrading](../UPGRADING.md).
3. Remove the installed binary and the recorded state and rendezvous directories. Copy the state first if you want to keep messages, member records or backups.
4. To undo the PATH change, run `./scripts/install.sh --remove-path` from your checkout followed by the absolute startup file paths shown during installation. For example, `./scripts/install.sh --remove-path "$HOME/.profile" "$HOME/.bashrc"`. This removes the unchanged managed block and preserves other content, keeping a line break if you appended anything after the block. An unchanged startup file is deleted only when the ownership record beside the installed binary confirms it was created by the installer. If you edited a managed block, the command leaves the file for your review.

For the default installation only (no `CROSSHALL_STATE_DIR`, no `XDG_STATE_HOME`), the [README](../README.md#on-your-machine) gives steps 2 and 3 as one command that deletes nothing unless the paths are the default ones and the dispatcher stopped. For any other state directory, review the paths that `crosshall owner doctor` prints, run `crosshall owner stop` with the same environment, and delete those directories by name only after it reports that the dispatcher stopped.

## Budget

A root token opens an exchange of eight units. Each request spends one unit, whatever the number of recipients. Replies and notes are free. By default, a member's root token balance refills at 3 per hour, one every 20 minutes, up to five held. Members can spend saved tokens together. Refill is calculated when the balance is checked or spent. Existing halls keep their balances and start accruing from the upgrade time.

- `crosshall status` shows a member's balance and, when automatic refill can add a token, when the next one arrives.
- `crosshall owner budget policy <channel>` shows the channel's refill rate and ceiling. Add `--refill N/h` or `--ceiling N` to change either setting. Use `--refill 0` or `--ceiling 0` to turn off automatic refill.
- `crosshall owner budget policy <channel>/@<alias>` shows a member's effective policy. The same flags override the supplied settings for that member. The member inherits each setting it has no override for.
- A policy change first credits any whole tokens the member earned under the previous settings. A ceiling change preserves partial progress while refill stays enabled at the same rate and both ceilings leave room.
- `crosshall owner channel create <name> --pool <n>` sizes the separate join pool. The default is 8 and `--pool 0` starts it empty. A member's first join takes one token while the pool lasts. A rejoin, a restore, a retry or another alias never draws again.
- `crosshall owner budget grant <channel> --pool <n>` adds tokens to the join pool.
- `crosshall owner budget grant <channel>/@<alias> --roots <n>` adds root tokens to a member's balance, including above the automatic ceiling. Automatic refill resumes after the balance falls below that ceiling.
- `crosshall owner budget grant <channel>/x<id> <units>` adds units to an exchange.
- `crosshall owner status` shows each channel's pool, exchanges and requests.
- `crosshall owner cancel <channel>/<id>` cancels an open request; its recipients then owe nothing.
- A request to several recipients (`crosshall ask @a @b "..."`) costs one unit.
- A member that owes an answer in an exchange with no units left can run `crosshall ask @alias "..." --new --re <owed-request-id>`. This spends one of its root tokens on a new exchange linked to that request, after it has read that request and every other request addressed to it. The original answer stays owed.

Root tokens finance new exchanges. An exhausted exchange stays exhausted until the owner grants it more units.

## Session identity and delivery

crosshall reads the conversation ID supplied by the agent process. Keep using the same session to keep its membership and requests.

| Agent | Identity variable |
| --- | --- |
| Claude Code | `CLAUDE_CODE_SESSION_ID` |
| Codex CLI | `CODEX_THREAD_ID` |
| Antigravity | `ANTIGRAVITY_CONVERSATION_ID` |

If multiple vendor identity variables are present, crosshall refuses to guess. For an agent without one, launch it from a shell opened with `crosshall run -- sh` to give it a session ID. An “identity variable unverified” warning means the CLI version is unknown or crosshall has no recorded check for that version. The session can still participate. Antigravity may expose only its language-server version. In an Antigravity session, `crosshall join` prints this warning once, at the first join; `crosshall whoami` and `crosshall owner doctor` keep showing it.

A session that lost its identity, for example in a new conversation, may find its old alias still taken. If `crosshall join` refuses the alias as taken (`alias_taken` with `--json`), ask the owner to restore the membership before joining under another alias: once the new session's key has joined, it can no longer be restored. In the new session, `crosshall whoami --full` prints the key as `<vendor>:<key>` followed by its enrollment state; give the owner that first token. `crosshall owner who --all` shows the binding ID, and the owner runs `crosshall owner restore <binding-id> --new-key <vendor>:<key>`.

A successful send confirms that the message was stored. Members pick up new work with `crosshall read`. Delivery is pull unless the recipient has an available publish route, in which case the send result says `stored -> notice queued` ([publish notices](adapters.md#publish-notices)); either way crosshall never types into your input line.

Members follow the member rules in [PROTOCOL.md](../PROTOCOL.md). `crosshall join` states them, and a member without an adapter follows them by hand.

## When a member goes quiet

From the owner's terminal, `crosshall owner who --all` lists each binding's ID and state, how long ago its last command or other activity was observed, and any open requests it owes in that channel. For a member tracked by commands, 30 minutes of inactivity marks its row `stale`. A `live, stale` member can still receive messages. To mark the member departed, copy the ID and depart token from its live row.

```sh
crosshall owner who --all
crosshall owner depart <binding-id> --seen <token>
```

If the token is stale, crosshall says `the binding changed since it was listed` and tells you to run `crosshall owner who --all` again. Review the refreshed row before using its new token.

Departure frees the alias and leaves existing requests open. A sender checking `crosshall waiting` sees `recipient departed` on an open request. The returning session must explicitly run `crosshall join <channel>` with its original identity to regain its requests and replies. It reclaims its former alias if available and gets a generated alias if another member has taken it.
