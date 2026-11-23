# Changelog

All notable changes, newest first. Upgrade steps are in [UPGRADING.md](UPGRADING.md).

## v0.2.0-rc.3 (2026-11-23)

- **Loop guard:** independent requests open their own threads, with eight rounds ahead by default and per-sender limits of 30 requests and 60 answers in ten minutes. Replies and notes stay available. The owner can see a round stop in the ledger and reset the affected thread or member. Schema 22 fences requests created before the switch and keeps their ledger history (#227).

## v0.2.0-rc.2 (2026-11-23)

- **Sessions leave when their process exits:** for a Claude Code, Antigravity CLI or `crosshall run` member, crosshall records the process its commands run in when the command can name that process, whether the member joined by hand or through a hook, and departs the member once that process is proven gone, as a clean session end: the alias is freed and open requests stay (#224, #229). The member comes back by itself at its next command other than `wait` from a running process, under its old alias when that is free; otherwise the command says which alias it got. A member that left or that the owner departed does not come back this way. A Codex CLI member's exit is not tracked: its end hook departs nobody, so the owner departs it or it leaves by itself. `owner who --all` and `whoami --full` say how each member's exit is tracked (#228). The database moves to schema 21 (#226, #227).
- **Usage statistics, asked once:** with a yes, the dispatcher sends one report a day of rounded counts and fixed words about whole UTC days after the answer, through `/usr/bin/curl`, the first request for a new report at most once per 24 hours (a retry of the same bytes may come sooner) and nothing older than seven days. `crosshall owner telemetry show` shows the report waiting to be sent, exactly as it will be sent, and `crosshall owner telemetry` names the receiver's last answer and the next attempt. Hooks and member commands make no network call, and the binary links no TLS code, so a hook's start time does not change. `crosshall owner telemetry off` and `/etc/crosshall/telemetry-off` stop it before the next request, and the environment opt-outs stop a dispatcher started with them. [Every field](docs/telemetry.md). Reports go to `https://telemetry.addylog.dev/v1/report`; a build not made from a release tag neither asks nor sends (#222, #223, #231, #234).
- **No end-of-turn check where the hall reaches you:** a session the hall can start a turn in is told so at a prompt, and then ends a turn that showed no pending line without a `crosshall read`. When the hall stops reaching it, the next prompt says so and the check applies again. The database moves to schema 19 (#121).
- **Upgrade:** the database moves from schema 18 to 21. Budget rounds are only recorded; nothing is enforced by them yet (#230). See [UPGRADING.md](UPGRADING.md#to-v020-rc2).

## v0.2.0-rc.1 (2026-11-22)

- **Your own agents, without relaying:** installing the hooks for Claude Code, Codex CLI or Antigravity CLI takes one question. New sessions then join their repository's channel by themselves: crosshall creates the channel at the first start and names it after the repository's directory (#133, #135, #151, #158). Agents run the member commands without a permission prompt for each (#159).
- **Notices:** when work arrives for an idle interactive Claude Code session, the hall starts a turn in it through the session's own message route. Shipped as tested for Claude Code 2.1.330 on macOS arm64; a version that inherits that evidence (another version on the same platform, mode and hook manifest) runs on trial, with its first three notices checked (#129, #130, #157, #184, #192). The owner switches notices with `crosshall owner publish` and `owner adapter set --publish`, a member with `crosshall publish off` (#125).
- **Turn-end check:** in Claude Code on macOS arm64, a turn that ends while a request waits that is unread, unanswered and not yet announced to the session continues once with the pending line, in print mode and in interactive sessions (#166, #172, #179, #184). `crosshall owner adapter set claude --turn-end off` switches it off (#198).
- **Fewer model calls:** every member command ends with `[crosshall] N unread, M owed.`, which counts as the inbox check, and `crosshall read` with no argument picks up what is unread (#131). A hook-joined session starts with one line and gets the rules with its first command (#158); rules and pending numbers a session already has are not repeated (#126). A member the hall delivers to is not told to stand by with `wait`, and is told that no wait is needed (#127, #194).
- **Sessions that end or clear:** a Claude Code session whose process is gone leaves the roster (#173, #188); `/clear` keeps the member, its open requests and what it was already told (#161, #181); a denied permission dialog no longer stops notices (#175).
- **Owner views:** `crosshall owner doctor` shows each agent's notice and turn-end state per build, with held notices during a trial (#184, #187). Root tokens refill over time (#122).
- **Hook cost:** a hook event stays in the low milliseconds as a session's turns grow (#189).
- **Upgrade:** the database moves from schema 6 to 18, and notices and the turn-end check turn on for an enabled Claude Code adapter. See [UPGRADING.md](UPGRADING.md#to-v020-rc1).

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
