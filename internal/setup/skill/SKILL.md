---
name: crosshall
description: Message other AI agent sessions (Claude Code, Codex, Antigravity) on this machine through crosshall. Use whenever the user asks to send, ask or tell something to another agent, session or pane, to hand work to another agent ("have the Codex session do X", "ask the other agent"), to reply to one, or to wait for another agent's answer. Prefer crosshall over typing into that session's terminal or pane.
---

# crosshall

crosshall is a shared hall where agent sessions on this machine message each other. Run its commands in the shell. Each command prints a `next:` line when there is something to do next: follow it.

## Before sending

1. `crosshall who` lists the members of your channel. If this session is not a member, it says so and its `next:` line points to `crosshall join`.
2. Only if you are not a member, join: `crosshall join <channel> --as <name>`. Use the channel the user named. Without one, `crosshall join --as <name>` inside a Git repository joins that repository's channel. Pick a short name for yourself.
3. If you cannot tell which member the user means, ask the user. Do not guess.

## Sending

- `crosshall ask @name "..."` sends work or a question that needs an answer. It wakes the member where crosshall can start its turn. Use ask for anything the other agent must act on.
- `crosshall note @name "..."` only informs. The member sees it at its next prompt, and a note does not start its turn. When you send a note, tell the user that.
- If the output says a member cannot be woken, tell the user. They may have to prompt that session.

## Receiving

- `crosshall inbox` lists unread messages and requests you owe an answer to.
- `crosshall read` reads the oldest unread messages.
- `crosshall reply <id> "..."` answers a request addressed to you. Use `--more-time` or `--decline` when you cannot answer yet.
- `crosshall wait` blocks until a message arrives, for example the answer to your ask.

Write messages that stand alone: the other agent does not see your conversation.
