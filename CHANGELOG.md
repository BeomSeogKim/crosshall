# Changelog

All notable changes, newest first. Upgrade steps are in [UPGRADING.md](UPGRADING.md).

## v0.1.0 (2026-11-20)

- **Clearer first steps:** `crosshall run -- sh` says that it started a member shell, and a nested one names the identity it replaces. Every refusal's `next:` line names a step that can be taken as printed, including the budget refusal, which now says whether you hold root tokens. The join hint carries rule 4 in full. `setup status` and `help` agree with `owner doctor`, `uninstall` says that it removes hooks only, and the installer's reasons name the tag and platform (#88).
- **Plainer messages:** `crosshall uninstall` says when no hooks are installed and changes nothing. The budget refusal says "no root tokens", alias-taken leads with the way for a new member, and a nested member shell says that exiting returns to the first identity. The installer tells a missing release archive (HTTP 404) from a release it could not reach (#89).
- **Conformance:** CONFORMANCE.md is measured again on v0.1.0-rc.13 (#87).
- **Docs:** the README is rebuilt for a newcomer, with examples that run in a test and an install example run against a local release; upgrade notes, adapters, waiting and operations have their own documents (#73).

## v0.1.0-rc.13 (2026-11-20)

- **Join pools:** each member's first join to a channel takes one root token from the channel's pool, which the owner sizes (`--pool`, default 8) and refills (`owner budget grant <channel> --pool <n>`). Existing channels get a pool of 0. The database moves to schema 5 (#83), and to schema 6 with linked handoffs.
- **Linked handoffs:** a member that owes an answer in an empty exchange can fund a handoff with `ask --new --re <owed-request-id>`; an open request is overdue only after its deadline instant (#84, #85).
- **Owner stop:** `crosshall owner stop` stops the dispatcher and waits for it to exit; `owner doctor` shows the state and rendezvous directories (#80).
- **Prebuilt binaries:** releases carry checksummed archives for macOS and Linux on arm64 and x86-64, and the installer uses them, with a source build as the fallback (#81).
- **Installer messages:** off a release tag, the installer names a local tag to switch to; after installing, it says what it installed, prints a PATH line when needed, and names the next command (#79).
- **Newcomer messages:** a newcomer's first mistakes get messages that say what to do (#78).
- **Rule 2 and C1:** rule 2 and the join hint say when to send `--more-time` (by the deadline, which itself counts, or on the next turn after a stopped session), and C1 says how to credit reads in an inbox snapshot (#86).
- **Docs:** PROTOCOL.md reads for a first reader (#75), and CONFORMANCE.md publishes member conformance runs per agent and version (#77).

## v0.1.0-rc.12 (2026-11-20)

- **Turn-end rule:** the join hint states it. Where a conformance run supports it, an adapter continues a turn that ends with an unread request, once per request (#51).
- **Member protocol:** PROTOCOL.md states the rules and guarantees and gives a shell-only conformance run (#54).
- **Results point to waiting work:** they show what waits for a member, with unread and owed counts and due times. `read` names the ways to answer (#59). The pointer prints only when something waits, and every suggested wait is 9 minutes (#63).
- **Repeated join:** it shows the hint again, the hint states the six member rules, and every refusal names a step that can succeed (#62).
- **Cancelled requests:** a request the owner cancelled before it was read is listed apart, and nothing continues a turn for it. A turn end does not wait where the check is unavailable (#60).
- **Owner start:** it reports success only for a dispatcher that answers. A stopping dispatcher no longer counts as started (#65).
- **Database:** it moves to schema 4.

## v0.1.0-rc.11 (2026-11-19)

- After setup refuses to pin Codex hooks from another state root, it names a step that can succeed (#46).

## v0.1.0-rc.10 (2026-11-19)

- A request costs one unit, whatever the number of recipients (#42).
- Setup pins the state root in the Codex hook commands it installs (#43).

## v0.1.0-rc.9 (2026-11-19)

- The owner sees stale members and can depart one binding (#37).
- `crosshall wait --existing` starts from unread messages, and `--work-only` waits for requests and replies only (#40).
- The database moves to schema 3.

## v0.1.0-rc.8 (2026-11-19)

- `crosshall wait` blocks until the next message arrives (#34).
- `waiting` and `status` show how long ago a request was picked up, and a taken alias points to owner restore (#33).
- The unknown Antigravity CLI version is reported once, at join (#35).

## v0.1.0-rc.7 (2026-11-19)

- The installer and `crosshall setup` offer adapter setup per vendor (#31).

## v0.1.0-rc.6 (2026-11-19)

- Reply reminder availability and hook counters are reported, and adapter costs are measured (#22). The reply reminder itself was later parked and is not in v0.1.0.

## v0.1.0-rc.5 (2026-11-19)

- Claude Code and Codex adapters, with setup, uninstall and conformance (#19).

## v0.1.0-rc.4 (2026-11-19)

- **Adapter core:** the hook entry point, observations and offered notices (#14).
- **Sandboxed members:** a sandboxed member can reach a running dispatcher (#16).

## v0.1.0-rc.3 (2026-11-19)

- Scenario, invariant, latency and rough-edge checks of the core (#11).

## v0.1.0-rc.2 (2026-11-18)

- `note`, `log`, `leave`, `focus`, and the owner's recovery commands (#8).

## v0.1.0-rc.1 (2026-11-18)

- **Store and dispatcher:** the store, the dispatcher and the transport, with `join` and `who` (#4).
- **Requests:** `ask`, `reply` and `read`, budgets, and the ledger (#6).
