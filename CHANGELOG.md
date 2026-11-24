# Changelog

All notable changes, newest first. Upgrade steps are in [UPGRADING.md](UPGRADING.md).

## Unreleased

- **A prompt absorbed into a running Claude Code turn no longer blocks notices:** Claude Code 2.1.330 takes a prompt typed while a tool runs into the running turn, so the hall's turn for it never ended and every later notice was `unknown`. Manifest `claude-3` also installs the Notification hook for `idle_prompt`; the session's idle report closes such turns and ends an unknown suspension recorded before it, timed or lasting, with a `publish_suspension_lifted` event. Rerun `crosshall setup claude` (#236).
- **Upgrade:** the database moves from schema 28 to 29. See [UPGRADING.md](UPGRADING.md#unreleased).

## v0.2.0-rc.4 (2026-11-24)

- **Session starts get more time under load:** a session start, and a prompt that enrolls or repairs its session, now answers within 400 ms instead of 150 ms; every other hook keeps 150 ms. A start makes three round trips to the dispatcher, and when other processes flush the disk each of them slows, so starts missed the 150 ms deadline and left the session unbound. A hook also picks up the dispatcher's reply every 1 ms instead of every 4 ms. `owner doctor` counts a missed deadline against the callback's own budget.
- **Move a session between channels:** `crosshall join <channel> --move` keeps the same session and moves its live membership. The receipt counts work left in the previous channel, where its requests and replies stay. A native notice awaiting its receipt holds the move until it resolves (#237).
- **A send no longer promises a turn end that will not offer the request:** a turn end offers at most 10 unread requests, so an 11th request to a member in a checked turn now says where it is seen otherwise (its next prompt or crosshall command, or `not woken`) instead of `it sees this when its turn ends`. `who` and `waiting` say the same.
- **Quiet turns end without waiting:** a Claude Code Stop waits for the dispatcher only while a request is left for its turn-end line; otherwise it records the turn end and returns. The dispatcher keeps that answer in a file of the member's own directory. On a loaded macOS machine this removes 13 to 15 ms at p50 from each quiet turn (#261).
- **`owner who` shows stuck requests:** a live member with unread requests that no notice, running wait or turn end will tell it about gets `; 2 unread requests, oldest 41m`, and one line above the channels names them all: `3 requests wait on members the hall cannot wake: @sol 2 (oldest 41m), @agy 1.` In JSON, the member carries `unread_not_woken`.
- **The store stops growing without bound:** once a UTC day the dispatcher deletes stored command results (`op`) and recorded commands and hook facts (`observation`) older than 7 days, keeping each session's latest hook result, newest record and newest command and every fact of a running session, and the turns of sessions whose facts are gone. It deletes at most 2,000 rows per transaction. A `--key` replays its stored result for 7 days (#256).
- **The owner hears of a lasting notice suspension:** a third suspension of a launch, which lasts until that launch ends, gets the round stop's desktop notification and a line on the next owner command, naming `crosshall owner publish resume <binding-id>`, while it still keeps the member's notices off. Every suspension, timed or lasting, now records a `publish_suspended` event. No schema change.
- **Dispatcher startup:** a new dispatcher retries when a status check briefly holds the state lock for reading. Concurrent starts still share one dispatcher (#102).
- **Owner start timeout:** `crosshall owner start` reports the last startup stage it observed and the state of the dispatcher lock and readiness marker when it times out (#102).
- **The owner hears of a round stop:** the dispatcher shows a desktop notification for a new stop, at most one a minute and none for a stop older than ten minutes, from release builds only; `CROSSHALL_NOTIFY=off` at `owner start` turns it off. The owner's next `crosshall owner` command prints each open stop once on stderr, leaving stdout and the exit status unchanged. `owner status` shows what a stop refused, the requests since the thread's last reset and the grant that lets it continue; its JSON gains `sender`, `recipients`, `requests`, `request_count` and `last_reset_at_ms`. Each grant records a `loop_guard_reset` event, and `owner budget grant <binding-id>` resets a departed member. Members see no change. Stops from before the upgrade are not announced (#227).
- **A pending line lost to a hook timeout shows again:** the prompt hook confirms a pending line after it writes it, and its IDs count as shown only then. IDs prepared for a hook that timed out before writing appear at the next eligible prompt, where they used to be left out of every later prompt line. `waiting` marks a prompt offer shown after the same confirmation. An older hook's offers still count when the dispatcher prepares them (#266).
- **Upgrade:** the database moves from schema 22 to 28. See [UPGRADING.md](UPGRADING.md#to-v020-rc4).
- **Doctor counts unknown receipts by cause:** each notice receipt classed `unknown` records why it was not the session's own turn (`open_placeholder`, `unfinished_turn`, `activity_since_stop` or `no_turn_but_facts`). `owner doctor` adds them to the day's notice line, for example `4 unknown (2 open placeholder, 1 activity since stop, 1 cause not recorded)`, and its JSON counters gain `unknown_causes`. Nothing else changes: an unknown receipt suspends notices as before (#236).
- **The send result says who will be told:** for each stored recipient, `notice queued`, or `not woken` with a fixed reason (no notice route, notices not proven for its build, suspended, off, the notice limit, an earlier notice unobserved, a note) and whether the recipient sees the message when its running turn ends (a request to a member whose turn-end check is on), at its next prompt or at its next crosshall command. When nothing will tell a recipient of a request or a final answer, one `next:` line says to ask a member the hall can wake or to tell the user. `crosshall who` and `crosshall waiting` use the same words, and `waiting` says `shown HH:MM, not read` for a request a prompt line or a turn end already showed, and `--json` carries `wake`, `why`, `seen_at` and `until_ms`. Usage reports count sends by outcome under `send.wake`, registry 2 (#236).
- **Codex routes follow the app server:** a Codex CLI session's hook reads whether the managed app server of its `CODEX_HOME` runs it, and records that server's release as the session's version. The route a Codex sender would use is open only then, from the session's start to its end; otherwise `owner who --all` says `publish: candidate (codex runs without its daemon)`. A notice's receipt from a Codex session is matched on its thread. A daemon start also names the server's process and home, which the dispatcher checks against the `CODEX_HOME` setup installed into before a Codex route could publish, and each Codex thread holds its own open notice. Codex still has no sender, so nothing starts a Codex turn yet (#236).
- **Lasting notice suspensions:** an unknown notice receipt suspends its session's notices for 30 minutes and no longer counts toward a lasting suspension. A third active suspension for the same launch lasts until that launch ends only if all three came from absorbed notices or failed submissions; an own-turn receipt or an owner resume resets the count. A suspension that an older dispatcher made lasting after an unknown receipt expires 30 minutes after it was recorded. A session told that it lost reach because of a lasting suspension is given `crosshall owner publish resume <binding-id>` for its user, and a session that already had the old line gets the command at its next prompt (#236).
- **A running wait is named:** a send to a member whose own `crosshall wait` is polling and accepts the message says `stored -> its wait is running` instead of `waits for its next crosshall command`, and `crosshall who` says `woken by its wait` for that member. A wait counts as running for three seconds after its last poll; the dispatcher keeps this in memory only. Rule 4 now asks for an untimed background wait where a background command's exit starts the next turn (Claude Code, and Antigravity CLI 1.5.15 in one probe), and a bounded one elsewhere; `docs/waiting.md` gives the pattern for each agent. A wait ends by itself, with `wait_host_gone`, within about five seconds of the end of the agent process it would name. A wait that times out in a session the hall delivers to now ends with `The hall brings new work to this session: no wait is needed.`, as documented; it used to name the wait command again (#236).
- **Usage statistics question:** an answer it does not accept is followed by `Answer y or n.` and the prompt line, not the whole paragraph again. `ㅛ` and `ㅜ`, the y and n keys under a Korean input method, count as y and n.

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
