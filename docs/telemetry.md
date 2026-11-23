# Usage statistics

crosshall can send one usage report a day to Adrian Studio, its maintainer, to decide what to fix and build. It is off until a person answers the question in their own terminal, and this document lists everything a report can hold. This build has no receiver configured, so it asks nothing and sends nothing.

## What is sent, and when

- **Only after a yes.** The setting has three states: unset, `on` and `off`. Only `on` leads to a report, and only about whole UTC days that began after the answer: an answer at 23:59 UTC sends nothing about that day. Turning it off and on again never brings back an earlier day. [How the question is asked](operations.md#usage-statistics).
- **One report per active day.** After a UTC day closes, the dispatcher builds that day's report once, writes it to `telemetry/pending/` in your state directory and never rebuilds it. A day with no activity has no report.
- **One request a day.** The dispatcher sends the first request for a new report at most once per 24 hours, at a random time up to six hours after the report was built, as one HTTPS POST made by `/usr/bin/curl`. A backlog goes out one report a day, oldest first; a report more than seven days old is deleted unsent. A failed request is retried with the same bytes after one hour, doubling up to a day; a retry may come sooner than 24 hours after the last request.
- **Only the dispatcher sends.** Hooks, member commands and the `crosshall` commands you type make no network call and gain no output. On a machine without `/usr/bin/curl` nothing is sent, and `crosshall owner telemetry` says why.
- **Visible before it is sent.** `crosshall owner telemetry show` prints the report waiting to be sent, byte for byte, and `crosshall owner telemetry` names the receiver's last answer and the next attempt.
- **Kept locally.** A sent report is moved to `telemetry/sent/` and kept 30 days. `crosshall owner telemetry sent` lists those days and `crosshall owner telemetry sent --day YYYY-MM-DD` prints one, byte for byte as it was sent.

## What a report holds

Rounded counts and fixed words, nothing else. Every count is rounded down to 1, 2, 5, 10, 20, 50, 100, 200, 500 or 1000, and a count of zero is left out. Every key and every word must be listed below: the program cannot encode anything else, and the receiver rejects a report that holds anything else.

A report never holds message text, channel names or topics, aliases, focus text, repository paths, session keys, message or launch references, process IDs, host names, user names, environment values, error text, full version strings, or anything that links one day's report to another's.

| Field | Value |
| --- | --- |
| `v` | `1`, the shape of the report |
| `registry` | `1`, the key list below |
| `batch` | 32 random hexadecimal characters drawn when the report is built. It only lets the receiver drop a copy sent twice; it is not reused and names nothing else |
| `day` | the UTC day counted |
| `app.version` | the crosshall release tag. A development build never reports |
| `app.platform` | `darwin-arm64`, `darwin-amd64`, `linux-arm64` or `linux-amd64` |
| `sources.hooks` | `ok`, or `missing` when the day's hook counter file was gone; then no `hook` key is sent |
| `sources.snapshot` | `taken` when the report was built within 10 minutes after the day closed, else `none`; only then are the `snap` keys and the switches sent |
| `switches.publish_hall`, `switches.turn_end_claude` | `on` or `off`: the owner's switch for notices and for the Claude Code turn-end check at the day's close |
| `counts` | the keys below |

### Counts

Each figure is counted on the day of its own event. `<A>` in a sentence below is the agent: Claude Code, Codex CLI or Antigravity CLI.

| Key | Meaning |
| --- | --- |
| `hook.{claude,codex,agy}.{start,prompt,stop,end,other}.{off,unmatched,identity,clean,rpc,deadline,refused,stale_epoch,no_dispatcher}` | hook runs of an agent, by event (session start, prompt, stop, session end, any other) and by outcome, from the hook counter file |
| `hook_ms.{claude,codex,agy}.{start,prompt,stop,end,other}.{lt5,lt10,lt28,lt50,lt150,ge150}` | the same hook runs by how long they took, in milliseconds |
| `msg.{ask,reply,note}` | messages sent, by kind |
| `ask_pair.{replied,declined,cancelled}` | requests settled for one recipient, by how |
| `pickup_lag.{lt10s,lt1m,lt10m,lt1h,lt1d,ge1d}` | requests picked up by a recipient, by the time since they were sent |
| `notice.reserved` | notices the hall prepared for a member |
| `notice.resolved.{submitted,not_submitted,uncertain}` | notices whose delivery attempt ended, by outcome |
| `notice.observed.{own_turn,absorbed,unknown}` | notices the agent's hook confirmed, by how the turn received them |
| `notice.retired_unobserved` | notice attempts closed without a confirmation |
| `notice.unobserved_10m` | notices submitted to the agent, or whose submission is uncertain, still unconfirmed 10 minutes later while their work was still waiting |
| `notice.late_observed` | notices of the previous figure that were confirmed later |
| `receipt.rejected.{malformed,stale}` | notice confirmations the hall rejected, by reason |
| `suspend.{absorbed,unknown,not_submitted}.{timed,lasting}` | notices paused for a session, by cause, and whether the pause had an end time |
| `turn_end.claim.{claude,codex,agy}` | turn-end checks of `<A>` committed to continue a turn with a pending line, counted even when no output followed |
| `join.{hook,cli,rejoined}` | channel joins made by a hook, by a command, and returns to a channel |
| `budget.exhausted` | requests refused because a member had no root tokens |
| `week.{active_days,unobserved_10m}` | over the seven days ending with this one, on this machine: days with a report, and the sum of `notice.unobserved_10m` |
| `snap.reach.{off,suspended,capped,held,candidate,available}` | at the day's close, joined members by whether the hall can bring them new work |
| `snap.build_stopped.{claude,codex,agy}` | at the day's close, agent builds whose notices are stopped |
| `snap.unread_ask_pairs` | at the day's close, open requests not yet picked up by a recipient |
| `snap.live_launch.claude.{2_1,other,unknown}` | at the day's close, running Claude Code sessions by release family |
| `snap.live_launch.codex.{0_170,0_171,other,unknown}` | at the day's close, running Codex CLI sessions by release family |
| `snap.live_launch.agy.{ls_1_5,other,unknown}` | at the day's close, running Antigravity CLI sessions by language server family |

A release family is the major and minor version of the agent that runs, `other` for one not listed, and `unknown` when the session did not report one. The machine-readable list is [`internal/telemetry/registry.json`](../internal/telemetry/registry.json).

## Where reports go

To `telemetry.addylog.dev`, the maintainer's own server, reached through Cloudflare's network. The server stores each report with the day it arrived, and stores no IP address, no request header and no time of day. A daily copy is encrypted before it is uploaded to the maintainer's Google Drive, through an app that can see only the files it created. A report stays on the server at most about 10 days, until its encrypted copy in Drive is verified, and then only in Drive, where it is deleted after 180 days. Reports are used only to decide what to fix and build in crosshall; they are not sold or shared. Anyone can post a report to the endpoint, so the figures are read with that in mind.

The same notice is published at [addylog.dev/crosshall/telemetry](https://addylog.dev/crosshall/telemetry/), with a way to contact Adrian Studio.

A report carries no identifier, but it is not unconnectable: the network path sees the address it came from, and with few users a rare platform or version can point to one machine. The counts describe how much one person or team worked with their agents that day.

Because no report carries an identifier, the receiver cannot find the reports of one person, so it cannot show or delete them on request; every report is deleted after 180 days. A report the receiver has accepted cannot be recalled. `crosshall owner telemetry off` stops every later one and deletes reports not yet sent.

## A day without a report

It can mean any of: the setting was off or unset, an opt-out was in effect, no dispatcher ran after the day closed, nothing happened that day, there was no network, or the report was dropped after seven days. Silence proves nothing.

## Turning it off

| Switch | Seen by | Effect |
| --- | --- | --- |
| `crosshall owner telemetry off` | the dispatcher, within a minute and before every request | off for this state directory; works with the dispatcher stopped and from unset |
| `/etc/crosshall/telemetry-off`, any content | the dispatcher, every minute and before every request; the question | off for every hall on the machine while it exists; reports not yet sent are deleted, and days on which a dispatcher saw it are never reported. The switch for a managed machine |
| `CROSSHALL_TELEMETRY=off` (or `0`, `false`, `no`, `disabled`), or `DO_NOT_TRACK` set to anything but empty, `0` or `false` | a dispatcher launched with it, for its whole life; setup and owner commands | for the dispatcher: no request, and no report about a day it ran on. For setup and owner commands: no question, and a setting of `on` becomes `off` |
| the same variables | hooks, member commands, a dispatcher already running | nothing: a variable set in one terminal is not a machine-wide switch |

A variable or file can only turn sending off.

## The request

`POST <endpoint>` with `Content-Type: application/json`, the report as the body (under 16 KiB), and `User-Agent: crosshall/<version>`. curl runs with an empty environment and ignores `~/.curlrc`; it uses HTTPS only, uses no proxy, follows no redirect, sends no cookie and gives up after 10 seconds.

| Answer | What the dispatcher does |
| --- | --- |
| 204 | stored (also for a copy sent twice): moves the report to `telemetry/sent/` |
| 400, 413, 415, 422 | the report was refused: deletes it |
| 410 | stops sending until crosshall is upgraded |
| 429, 5xx, a redirect, any other answer or none | sends the same bytes again later, honouring `Retry-After` up to a day |

The body is canonical: no whitespace, members in the order of the table above, counts in ascending key order.

## Changes

A new count or word needs an issue, a new registry number, a changelog entry and a row here. A new kind of data, or any identifier, would reset every installation to unset and ask again.
