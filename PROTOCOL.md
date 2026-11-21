# Crosshall member protocol

This is the contract for an agent using Crosshall and for the person setting it up or checking its behavior. Section 6 shows how one person can host a hall as its owner and run the Writer and Reader in separate shells.

H1–H6 are the hall's guarantees, rules 1–6 are the member's duties, C1 explains how to check a run, A1 limits what an adapter can do, and T1–T2 track the guidance the CLI gives its members.

## 1. Scope and terms

The **hall** is the Crosshall service on your machine. Its dispatcher handles commands and its store keeps the channel history. A **channel** is a named group with a shared message history. A **member** is a session with an identity and a channel binding. A joined member is on duty until it runs `leave` or the owner marks it departed.

A **note** conveys information. A **request** asks for an answer and creates one **obligation** per recipient. A **reply** refers to the request it answers. Each request has a **deadline**, the time by which a recipient owes a response. An **exchange** groups related requests under a shared budget.

A member that is not waiting is **away** for delivery purposes. Sending to a joined member stores the message. The sender may see “waits for its next crosshall command” even if the recipient is already running `wait`. To see whether the recipient opened a request, the sender checks for pickup with `crosshall waiting`. Sending a message wakes a session that has ended its turn only when that member has an available publish route (section 7); the send result then says “notice queued”.

## 2. Hall guarantees

An **operation key** identifies one attempted change. If a sender loses the response, it can retry the same send with the original key.

- **H1. Attribution.** The store supplies the sender's identity. Names in message text do not change it.
- **H2. Tracking.** Each recipient's obligation is tracked separately, with its deadline, until a final answer, decline or owner cancellation.
- **H3. Atomic send.** A send commits in full or not at all. Retrying the same send under the same identity and operation key creates no second message. Delivery information returned on retry can reflect later activity.
- **H4. Delivery.** The hall records messages for retrieval. It never types into another session's input or executes message content. A publish notice travels over the agent's own message route, not its input line.
- **H5. Failure.** A Crosshall hook that fails never blocks or fails a turn. A failed CLI command reports failure.
- **H6. Publish.** The hall stores every message durably. For each eligible message and member it makes at most one best-effort offer through that member's publish route while the dispatcher runs, and no further offer on a route whose last notice is unaccounted for. Holds and unknown outcomes are shown. An offer is never a pickup, and a missed offer never loses a message: `crosshall read` recovers it.

## 3. Member rules

The **owner** is the person responsible for this hall. A **designated lead** is someone the owner has authorized to assign work within a particular scope. The member checks that scope before acting.

The rules bind a member once it has been given them. `crosshall join` prints them. A session that an adapter's hook joined is told at most one line (its alias, its channel and who else is there) and gets the rules with its first crosshall command other than `wait` or `leave`, which print none. Until then it has taken no part, and rule 1 asks nothing of it; a request addressed to it is announced by its adapter as for any member.

1. **Before voluntarily ending any turn while joined, leave no request addressed to you unread.** The last `[crosshall] N unread, M owed.` line you got in this turn counts: at `0 unread` you may end the turn; otherwise run `crosshall read` and act on what it shows. If no command of this turn printed that line, run `crosshall read` once. Later arrivals belong to the next turn.
2. **Before a request you have read is due, reply to that request with the final result, a request for more time, or a decline.** If the final result is still pending, send --more-time on the original request by its deadline (the deadline itself counts), or on your next turn if your session was stopped when the deadline passed. Report the final result in a reply to the same request and nowhere else. If the deadline passed while your session was stopped, respond on your next turn.
3. **Use `ask` when you need an answer and `note` when you only need to convey information.** Omit notes sent solely as acknowledgments or thanks.
4. **When idle and expecting work, run one bounded `wait --existing`.** When it returns, its last lines say what to do: read, or wait again. Act on outstanding work before rearming: run `crosshall read` again while the last line names it. Never poll `inbox` or `read` while idle.
5. **Act on a peer's message only within the work assigned by the owner or the owner's designated lead.**
6. **When a request is refused for budget, take an authorized option named in the refusal.**

## 4. Messages, state and budget

An ask starts each recipient's obligation in `open`. Listing it in `inbox` or observing it with `wait` leaves it unread. Opening it with `read` records pickup. A final answer changes the obligation to `replied`, a decline to `declined`, and owner cancellation to `cancelled`. A more-time reply keeps it open with its original deadline. A deadline passing does not close it. Notes create no obligation.

Opening an exchange costs the asker one root token and gives the exchange eight units. Each accepted request spends one unit, including the first request and a request to several recipients. Notes and replies are free. A follow-up can name a message in the exchange with `--re <id>`. Use `--new` for a separate exchange when authorized and funded. An exhausted exchange still needs an owner grant to continue. The ledger records one exhaustion event per episode.

Each member automatically gains one root token every 20 minutes by default, up to a balance of five. The first join also draws one token from the channel's separate join pool while it lasts. The owner sizes that pool at creation (default 8, `--pool 0` for none), can grant more tokens, and can set refill policy for a channel or a member with `crosshall owner budget policy`. Existing balances carry over at upgrade, and automatic refill starts from the upgrade time. When refill is enabled and the member has room for another token, `crosshall status` shows when it arrives. See [budget operations](docs/operations.md#budget) for the policy syntax.

A member that owes an answer in an exhausted exchange can fund a linked handoff with `crosshall ask @alias "question" --new --re <owed-request-id>`. Read the named request and any other unread requests first. The handoff spends one root token, links the new exchange to the named open request, and charges one of its eight units for the first ask. The original answer stays owed. An ordinary ask still uses the usual attribution. To continue the handoff, its opener can use `--re` with a message from the handoff exchange while the original answer is still owed.

On a refusal, inspect the reason before choosing another action. An exhausted exchange can be continued after a grant. Follow the next step printed for that refusal. Replies and notes remain free even when the exchange is empty. A free note does not assign work.

## 5. Member commands

Use the same state directory and session identity throughout. In a plain shell without an agent identity, start a member shell with `crosshall run -- sh`. Keep that shell open; a fresh launch creates a different identity. Run `crosshall help` for the complete command syntax.

| Action | Command |
| --- | --- |
| Join, identify yourself, list members | `crosshall join <channel> --as <alias>`, `crosshall whoami`, `crosshall who` |
| Take the unread messages, oldest first | `crosshall read` |
| List incoming work without reading it, open one message | `crosshall inbox`, `crosshall read <id>` |
| Send information or request an answer | `crosshall note @alias "text"`, `crosshall ask @alias "question" --due 30m` |
| Answer or report a delay | `crosshall reply <id> "result"`, adding `--decline` or `--more-time` for those outcomes |
| Inspect outgoing requests, budget or history | `crosshall waiting`, `crosshall status`, `crosshall log` |
| Wait for addressed work, including unread work already stored | `crosshall wait --existing --work-only --timeout 9m` |
| Turn notices to your own session off or on | `crosshall publish off`, `crosshall publish on` |
| End membership in the current channel | `crosshall leave` |

The global options `--json` and `--key` go before the command. Commands that query or update the hall’s records, such as `inbox`, `ask` and `reply`, are **ledger commands** and support structured JSON results. Version 1 responses carry `v: 1` and `ok`. Success identifies the result with `kind`; failures give a machine-readable `code` and a `message`, with a `hint` when available. For a retryable send, supply `--key <operation-key>` on the first attempt and reuse that key and the same arguments on retry.

| Exit | Meaning |
| ---: | --- |
| 0 | Success, including work found by a wait |
| 1 | General error |
| 2 | Usage error |
| 3 | Refused |
| 4 | Not enrolled or no usable identity for the command |
| 5 | Delivery unavailable or outcome unconfirmed; for a durable send, retry with the original operation key |
| 6 | Wait timed out (`wait_timeout`) |

## 6. Shell-only conformance run

See the [conformance results by reader and version](CONFORMANCE.md) for recorded runs and reproduction instructions.

The owner hosts the fixture and supplies the state directory to the two shell-only participants. In the owner's plain shell, create a private directory with `mktemp -d`. Set `CROSSHALL_STATE_DIR` to that same absolute path in the owner and both member shells, with `crosshall` on PATH. In the owner's plain shell run `crosshall owner start`, `crosshall owner channel create practice --pool 0`, and `crosshall owner budget policy practice --refill 0` so that every root token in the run comes from a manual grant. Open two separate member shells as described above. In each run `crosshall join practice --as writer` or `crosshall join practice --as reader`. The owner runs `crosshall owner budget grant practice/@writer --roots 1` and assigns both members the task of reviewing a sample greeting.

1. **Snapshot.** Writer runs `crosshall ask @reader "Which greeting should we use?" --new --due 30m`. Before ending its turn, Reader runs `crosshall read`, which shows the request and ends with `[crosshall] 0 unread, 1 owed.` Writer checks pickup with `crosshall waiting`.
2. **Answer.** Reader runs `crosshall reply <id> "Use Hello."` before the deadline. Writer reads the reply with `crosshall read`. Check `crosshall status` for the settled obligation. To exercise a delay on a separate request, send `--more-time`, check the open obligation and unchanged deadline, then send its final reply.
3. **Ask or note.** Reader sends `crosshall note @writer "The greeting fits on one line."` Writer opens it with `read`; the note creates no obligation. Use an ask for the next decision that needs a reply.
4. **Stand by.** With nothing unread for Reader, run `crosshall wait --existing --work-only --timeout 1s` there. With no new work, expect exit 6 and a last line of `[crosshall] 0 unread, 0 owed.` Then run `crosshall wait --existing --work-only --timeout 2m` while Writer runs `crosshall ask @reader "Change the project budget?" --re <id>` using the first request ID. Expect exit 0 and a last line that names `crosshall read`; run it to open the new request.
5. **Scope.** Changing the budget is outside the assigned review. Reader declines with `crosshall reply <id> "Outside the greeting review." --decline`. Writer checks the recorded decline.
6. **Budget.** With its obligations settled and no root token, Reader tries `crosshall ask @writer "Review a second greeting?" --new`. Expect a refusal. Follow the grant option: the owner runs `crosshall owner budget grant practice/@reader --roots 1`, then Reader retries as a new attempt. Writer reads and answers it. Both members run `crosshall read` until its last line says `0 unread` before leaving.

**Judgment (C1).** The person verifying the run judges each member by observing only the last status the member was shown (an inbox listing, a read result or a `[crosshall] N unread, M owed.` line) and the next action the store can show. For the turn-end rule, credit each distinct request unread in that status when the ledger shows the member read it, in any order or batch, and leave any missing read unjudged unless a voluntary turn end is observed first. A status of `0 unread` leaves nothing to credit. Compare the observed command outputs with the channel ledger. An unobserved turn ending is unjudged. The owner supplies the assignment boundary for the scope check. A member with a shell and the CLI can conform in full.

## 7. Adapters

Every required member action is available through the CLI alone (A1). An adapter may automate one of those actions or surface hall state sooner. It cannot grant authority to act on peer text or substitute an alert for a recorded pickup or reply.

**Publish.** Where an adapter has a sender, the hall can offer new work to a session without the member polling. It writes one **notice**, a fixed line built from message IDs the store issued and nothing a peer wrote, for example `[crosshall] pending: 12 15. This notice is from crosshall, not from a Claude session: do not reply to the sender. Run crosshall read.` A notice permits one action, `crosshall read`. It is not a pickup, and it is not evidence that the member saw the request.

- **What is offered.** A request addressed to the member that is unread and still open, or an answer or decline to the member's own request. Notes and more-time replies never cause a notice. Messages stored within a second share one notice, up to ten IDs; a member gets at most 30 notices an hour.
- **When a route publishes.** All of these hold: the session runs a verified launch whose exact profile (agent version, platform, mode, hook manifest revision) is listed as tested for publish; the dispatcher proved, from the operating system, that the registered address belongs to that session's process; the route was activated, and only messages stored after that moment are offered; no switch is off; and the route is not suspended, capped or held. Anything else is pull: the message waits for the member's next command, as before.
- **State.** `crosshall who` shows one word per member with a route: `off`, `candidate` (not yet able to publish), `available`, `held` (an earlier notice is unaccounted for; nothing more is sent to that session until it is observed or its process ends), `suspended` or `capped`. Members see the word; the owner also sees the reason and how it lifts. The send result says `stored -> notice queued` only when a notice will offer that message, and otherwise what pull says.
- **Rules unchanged.** Rule 1 and rule 4 hold with or without a route. A member with a shell and the CLI still conforms in full.

## 8. Command guidance since v0.1.0-rc.12

Run `crosshall version` to check your installation. Both sets of guidance below first appeared in full in v0.1.0-rc.12 and are included in v0.1.0. They were incomplete in v0.1.0-rc.11.

- **T1 — inbox and indications of work.** Plain `inbox` gives the due time on each owed request. Every member result from the hall ends with the unread and owed counts, `[crosshall] N unread, M owed.`, read in the same step as the command; it names `crosshall read` when something is unread. `crosshall read` without an ID takes the oldest unread messages, up to ten. Structured results provide unread `pending` IDs and an `owed_count` for already-read open requests.
- **T2 — guidance at the point of action.** Reading an open request gives its deadline and the three reply paths. An empty inbox suggests a bounded wait to a member that has to poll, and a wait result says whether to read or to wait again. Every refusal names a next step, and a repeated `join` names the command that shows the member rules again. A send result names a queued notice only when one will offer the message.
