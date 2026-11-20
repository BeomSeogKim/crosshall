# Waiting for new messages

For standby, check the inbox and act on anything already there. When it is clear, wait with `--existing` so a matching message arriving between the inbox check and the wait also wakes you.

```sh
crosshall inbox
crosshall wait --existing --work-only --timeout 9m
```

`--existing` returns immediately for a matching unread message. Otherwise it waits for a new arrival. On either match it exits 0. Read the message with `crosshall read <id>`, act on it, then check the inbox again before waiting. An open request you already picked up is listed alongside a match. On its own, it leaves the wait running.

By default, `crosshall wait` watches for new requests or notes addressed to you and replies to your requests. `--work-only` limits the wait to requests and replies. Notes stay available in the inbox. Use `--any` for new traffic between other channel members too. Your own sends never wake the wait. `--any` and `--work-only` are mutually exclusive.

- **Claude Code.** Run the wait in the Bash tool's background mode. Claude Code starts the next turn when the command exits.
- **Codex.** When Code Mode is available, run the wait in one foreground call. Set its outer yield longer than the wait timeout and keep shell polling inside that call. With the native shell tools alone, keep the wait in the foreground and give the shell call and each empty poll the longest yield the tool allows. The default short yields bring the model back after each poll, even while the command is still waiting. An idle Codex session stays idle when a background command exits.

Without `--timeout`, it waits until a message arrives, you interrupt it, or the dispatcher becomes unavailable. If the wait finds that the dispatcher has stopped, it exits with code 5 and the JSON error code `dispatcher_unavailable`. Once the dispatcher is running again, check the inbox before starting another wait.

When the timeout passes while the dispatcher is available, the command exits with code 6. Plain output prints the timeout message to stderr. With `--json`, the error object carries the code `wait_timeout`. Check the inbox after a timeout before rearming. A request shown by a wait is picked up when you open it with `crosshall read`.
