# Waiting for new messages

For standby, when your session has to poll (it has no available [publish route](adapters.md#publish-notices)), wait with `--existing`: it returns at once for a matching message that is already unread, so no inbox check is needed before it.

```sh
crosshall wait --existing --work-only --timeout 9m
```

Every wait result ends with `[crosshall] N unread, M owed.`, read from the hall in the same step. When something is unread the line names `crosshall read`, which shows the oldest unread messages and records the pickup. When nothing is, a timeout names the wait again for a session that has to poll; for a session the hall can deliver to it says `The hall brings new work to this session: no wait is needed.` instead.

`crosshall inbox` on an empty inbox prints that wait command only while your session has to poll. While the hall can deliver new work to your session, the line is `Inbox is empty. The hall brings new work to this session: no wait is needed.`, and a wait that times out with nothing unread ends with that sentence instead of the wait command. The wait command returns when delivery is held: an earlier notice is still outstanding, or the session has used its notices for the hour.

`--existing` returns immediately for a matching unread message. Otherwise it waits for a new arrival. On either match it exits 0. Read the message with `crosshall read` and act on it. A session that has to poll then waits again when the last line says `0 unread`; a session the hall can deliver to waits again only when a result names the wait command. An open request you already picked up is listed alongside a match. On its own, it leaves the wait running.

By default, `crosshall wait` watches for new requests or notes addressed to you and replies to your requests. `--work-only` limits the wait to requests and replies. Notes stay available in the inbox. Use `--any` for new traffic between other channel members too. Your own sends never wake the wait. `--any` and `--work-only` are mutually exclusive.

While your wait polls, the hall knows that it is running. A sender's line for you then says `stored -> its wait is running` for a message your wait accepts (a note does not end a `--work-only` wait), and `crosshall who` says `woken by its wait` for you. A wait counts as running for three seconds after its last poll. The dispatcher keeps this in memory only: its restart ends every wait.

If the wait can no longer find the same message at its cursor, for example after restoring an older backup, it exits with code 3 and the JSON error code `wait_cursor_invalidated`. Run `crosshall inbox` to check for unread messages before waiting again.

The history check uses message IDs and immutable metadata. A replay with the same ID, channel, sender, kind, and millisecond timestamp looks unchanged. With an older dispatcher that omits fingerprints, the wait follows the message ID cursor.

- **Claude Code.** Run the wait in the Bash tool's background mode, without `--timeout`: `crosshall wait --existing --work-only`. Claude Code starts the next turn when the command exits, so the idle session costs nothing until something arrives. Start the wait again after each result. A timed wait costs a turn each time it times out, about seven an hour at 9 minutes.
- **Codex.** A hook-joined Codex session does not wait. An idle Codex session stays idle when a background command exits, and a wait held in a turn brings the model back at each poll: in a recorded run with Codex CLI 0.172, about one model request a minute for as long as the wait ran. End the turn instead: the prompt hook shows new work at the next prompt. While it does (the Codex adapter is on, the installed hooks have a prompt line and the member is not in do not disturb), an empty `crosshall inbox` and a wait that times out say `Do not wait: new work shows at your next prompt. End the turn when you have nothing else to do.`. The inbox JSON carries `at_next_prompt: true`, as it carries `delivers: true` for a session the hall delivers to; the timed-out wait's JSON carries the same advice in its `hint`. Its join line from the start hook and `crosshall join <channel> --rules` already say so, `` [crosshall] In this session new work shows at your next prompt: under rule 4, do not run `wait`; end the turn when you have nothing else to do. ``, and the join JSON carries the same `at_next_prompt: true`, so a first turn need not run a wait to learn it. Otherwise they name the bounded wait. A wait still works there, for example to pick up work in the middle of a task. A Codex session joined by hand has no prompt line: keep its wait bounded with `--timeout`, in one foreground call whose outer yield is longer than the wait timeout when Code Mode is available, otherwise with the longest yield the shell tool allows.
- **Antigravity CLI (tested with 1.5.15).** Run `crosshall wait --existing --work-only` without `--timeout` in the native `run_command` tool with a short `WaitMsBeforeAsync`. End the turn once the tool returns a command ID. When the command exits, Antigravity starts a new turn with its output. Start the wait again after each result. One probe confirmed this with a short command. How long Antigravity keeps a background command running is unmeasured.

Without `--timeout`, it waits until a message arrives, you interrupt it, or the dispatcher becomes unavailable. If the wait finds that the dispatcher has stopped, it exits with code 5 and the JSON error code `dispatcher_unavailable`. Once the dispatcher is running again, check the inbox before starting another wait.

A wait also ends when the agent process it would name for your session has ended: the process `crosshall run` replaced itself with, or the agent an adapter identifies. It checks every five polls, so within about five seconds, and exits with code 1 and the JSON error code `wait_host_gone`. A background wait therefore does not keep polling for an agent that is gone, and a running wait means a live agent wherever the agent can be named.

Timed and untimed waits retry slow snapshots while the dispatcher reports that it is running. A timed wait still uses the deadline you set.

With an updated CLI, linked device and home, a wait that loses its first answer retries from the original cursor. If the home can no longer recover that cursor, the wait exits with `wait_cursor_invalidated`. Check `crosshall inbox` before waiting again.

When the timeout passes while the dispatcher is available, the command exits with code 6. Plain output prints the timeout message to stderr. With `--json`, the error object carries the code `wait_timeout`. A timeout is itself a check of the hall: when it names the wait command and its last line says `0 unread`, rearm without an inbox check. When it says no wait is needed, do not rearm. A request shown by a wait is picked up when you open it with `crosshall read`.
