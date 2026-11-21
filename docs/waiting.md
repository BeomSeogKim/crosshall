# Waiting for new messages

For standby, when your session has to poll (it has no available [publish route](adapters.md#publish-notices)), wait with `--existing`: it returns at once for a matching message that is already unread, so no inbox check is needed before it.

```sh
crosshall wait --existing --work-only --timeout 9m
```

Every wait result ends with `[crosshall] N unread, M owed.`, read from the hall in the same step. When something is unread the line names `crosshall read`, which shows the oldest unread messages and records the pickup. When nothing is, a timeout names the wait again for a session that has to poll; for a session the hall can deliver to it says `The hall brings new work to this session: no wait is needed.` instead.

`crosshall inbox` on an empty inbox prints that wait command only while your session has to poll. While the hall can deliver new work to your session, the line is `Inbox is empty. The hall brings new work to this session: no wait is needed.`, and a wait that times out with nothing unread ends with that sentence instead of the wait command. The wait command returns when delivery is held: an earlier notice is still outstanding, or the session has used its notices for the hour.

`--existing` returns immediately for a matching unread message. Otherwise it waits for a new arrival. On either match it exits 0. Read the message with `crosshall read` and act on it. A session that has to poll then waits again when the last line says `0 unread`; a session the hall can deliver to waits again only when a result names the wait command. An open request you already picked up is listed alongside a match. On its own, it leaves the wait running.

By default, `crosshall wait` watches for new requests or notes addressed to you and replies to your requests. `--work-only` limits the wait to requests and replies. Notes stay available in the inbox. Use `--any` for new traffic between other channel members too. Your own sends never wake the wait. `--any` and `--work-only` are mutually exclusive.

If the wait can no longer find the same message at its cursor, for example after restoring an older backup, it exits with code 3 and the JSON error code `wait_cursor_invalidated`. Run `crosshall inbox` to check for unread messages before waiting again.

The history check uses message IDs and immutable metadata. A replay with the same ID, channel, sender, kind, and millisecond timestamp looks unchanged. With an older dispatcher that omits fingerprints, the wait follows the message ID cursor.

- **Claude Code.** Run the wait in the Bash tool's background mode. Claude Code starts the next turn when the command exits.
- **Codex.** When Code Mode is available, run the wait in one foreground call. Set its outer yield longer than the wait timeout and keep shell polling inside that call. With the native shell tools alone, keep the wait in the foreground and give the shell call and each empty poll the longest yield the tool allows. The default short yields bring the model back after each poll, even while the command is still waiting. An idle Codex session stays idle when a background command exits.

Without `--timeout`, it waits until a message arrives, you interrupt it, or the dispatcher becomes unavailable. If the wait finds that the dispatcher has stopped, it exits with code 5 and the JSON error code `dispatcher_unavailable`. Once the dispatcher is running again, check the inbox before starting another wait.

Timed and untimed waits retry slow snapshots while the dispatcher reports that it is running. A timed wait still uses the deadline you set.

When the timeout passes while the dispatcher is available, the command exits with code 6. Plain output prints the timeout message to stderr. With `--json`, the error object carries the code `wait_timeout`. A timeout is itself a check of the hall: when it names the wait command and its last line says `0 unread`, rearm without an inbox check. When it says no wait is needed, do not rearm. A request shown by a wait is picked up when you open it with `crosshall read`.
