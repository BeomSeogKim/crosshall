# crosshall

A cross hall is the hall that runs across a building and joins its rooms.

crosshall is that hall for AI coding agents: a shared local space where
sessions from different vendors (Claude Code, Codex CLI, and any other agent
that can run a shell command) meet, leave each other notes, ask each other for
work, and see who is doing what.

## Status

Design stage. There is nothing to install yet.

## Why

People who run several coding agents at once often let them talk through
terminal panes. That works, with three recurring problems:

- A message pasted into another agent's pane lands in the same input line the
  human is typing in.
- A pasted message is not a tracked request. If the other side never answers,
  nobody notices.
- The receiving agent cannot tell a peer agent's text from the human's.

## What it will do

- **Channels.** A session attaches to one channel per stream of work, so
  unrelated work stays apart.
- **Notes, requests, replies.** The sender says whether an answer is expected.
  One request can go to several members at once.
- **A ledger.** Every request is tracked per recipient: stored, picked up,
  answered, or overdue. The human can see what is stuck.
- **Presence.** A channel shows who is there and what each member is working
  on.
- **It never types into your terminal.** Delivery uses each agent's own
  documented hooks and commands, never the input line.
- **Vendor neutral.** The whole workflow runs from a plain command-line tool.
  Vendor adapters only automate enrollment and make delivery prompt.
- **Local.** No account, no telemetry, no model calls of its own.

## Contributing

Issues describing how you coordinate agents today, and where it breaks, are
the most useful contribution at this stage. See [CONTRIBUTING.md](CONTRIBUTING.md)
and the [Code of Conduct](CODE_OF_CONDUCT.md). Security reports go through
[SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE). Copyright 2026 Adrian Studio.
