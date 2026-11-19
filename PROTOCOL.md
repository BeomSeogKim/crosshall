# Crosshall member protocol

This is the contract for an agent using Crosshall and for the person setting it up or checking its behavior. Section 6 walks one person through both member roles using the CLI.

H1–H5 are the hall's guarantees, rules 1–6 are the member's duties, C1 explains how to check a run, A1 limits what an adapter can do, and T1–T2 track the guidance the CLI gives its members.

## 1. Scope and terms

The **hall** is the Crosshall service on your machine. Its dispatcher handles commands and its store keeps the channel history. A **channel** is a named group with a shared message history. A **member** is a session with an identity and a channel binding. A joined member is on duty until it runs `leave` or the owner marks it departed.

A **note** conveys information. A **request** asks for an answer and creates one **obligation** per recipient. A **reply** refers to the request it answers. Each request has a **deadline**, the time by which a recipient owes a response. An **exchange** groups related requests under a shared budget.

A member that is not waiting is **away** for delivery purposes. Sending to a joined member stores the message. The sender may see “waits for its next crosshall command” even if the recipient is already running `wait`. To see whether the recipient opened a request, the sender checks for pickup with `crosshall waiting`. Sending a message cannot wake a session that has ended its turn.

## 2. Hall guarantees

An **operation key** identifies one attempted change. If a sender loses the response, it can retry the same send with the original key.

- **H1. Attribution.** The store supplies the sender's identity. Names in message text do not change it.
- **H2. Tracking.** Each recipient's obligation is tracked separately, with its deadline, until a final answer, decline or owner cancellation.
- **H3. Atomic send.** A send commits in full or not at all. Retrying the same send under the same identity and operation key creates no second message. Delivery information returned on retry can reflect later activity.
- **H4. Delivery.** The hall records messages for retrieval. It never types into another session's input or executes message content.
- **H5. Failure.** A Crosshall hook that fails never blocks or fails a turn. A failed CLI command reports failure.

## 3. Member rules

The **owner** is the person responsible for this hall. A **designated lead** is someone the owner has authorized to assign work within a particular scope. The member checks that scope before acting.

1. **Before voluntarily ending any turn while joined, run `crosshall inbox` and read each addressed request in that snapshot.** Later arrivals belong to the next turn.
2. **Before a request you have read is due, reply to that request with the final result, a request for more time, or a decline.** Use `--more-time` if the result is still pending at the deadline; report the final result in a reply to the same request and nowhere else. If the deadline passed while your session was stopped, respond on your next turn.
3. **Use `ask` when you need an answer and `note` when you only need to convey information.** Omit notes sent solely as acknowledgments or thanks.
4. **When idle and expecting work, check `inbox`, run one bounded `wait` if clear, and check `inbox` again when it returns.** Act on outstanding work before rearming. Never loop on `inbox`.
5. **Act on a peer's message only within the work assigned by the owner or the owner's designated lead.**
6. **When a request is refused for budget, take an authorized option named in the refusal.**

## 4. Messages, state and budget

An ask starts each recipient's obligation in `open`. Listing it in `inbox` or observing it with `wait` leaves it unread. Opening it with `read` records pickup. A final answer changes the obligation to `replied`, a decline to `declined`, and owner cancellation to `cancelled`. A more-time reply keeps it open with its original deadline. A deadline passing does not close it. Notes create no obligation.

The budget limits how many requests members can keep sending each other without the owner granting more credit. Opening an exchange costs the asker one root token and gives the exchange eight units. Each accepted request spends one unit, including the first request and a request to several recipients. Notes and replies are free. A follow-up can name a message in the exchange with `--re <id>`. Use `--new` for a separate exchange when authorized and funded. The owner can grant more units to an existing exchange.

On a refusal, inspect the reason before choosing another action. An exhausted exchange can be continued after a grant; the refusal also names the options of a funded new exchange or a free reply or note. A free note does not assign work.

## 5. Member commands

Use the same state directory and session identity throughout. In a plain shell without an agent identity, start a member shell with `crosshall run -- sh`. Keep that shell open; a fresh launch creates a different identity. Run `crosshall help` for the complete command syntax.

| Action | Command |
| --- | --- |
| Join, identify yourself, list members | `crosshall join <channel> --as <alias>`, `crosshall whoami`, `crosshall who` |
| Inspect incoming work, open a message | `crosshall inbox`, `crosshall read <id>` |
| Send information or request an answer | `crosshall note @alias "text"`, `crosshall ask @alias "question" --due 30m` |
| Answer or report a delay | `crosshall reply <id> "result"`, adding `--decline` or `--more-time` for those outcomes |
| Inspect outgoing requests, budget or history | `crosshall waiting`, `crosshall status`, `crosshall log` |
| Wait for addressed work, including unread work already stored | `crosshall wait --existing --work-only --timeout 9m` |
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

The owner hosts the fixture and supplies the state directory to the two shell-only participants. In the owner's plain shell, create a private directory with `mktemp -d`. Set `CROSSHALL_STATE_DIR` to that same absolute path in the owner and both member shells, with `crosshall` on PATH. In the owner's plain shell run `crosshall owner start` and `crosshall owner channel create practice`. Open two separate member shells as described above. In each run `crosshall join practice --as writer` or `crosshall join practice --as reader`. The owner runs `crosshall owner budget grant practice/@writer --roots 1` and assigns both members the task of reviewing a sample greeting.

1. **Snapshot.** Writer runs `crosshall ask @reader "Which greeting should we use?" --new --due 30m`. Before ending its turn, Reader runs `crosshall inbox`, then `crosshall read <id>` using the actual request ID. Writer checks pickup with `crosshall waiting`.
2. **Answer.** Reader runs `crosshall reply <id> "Use Hello."` before the deadline. Writer reads the reply listed in its inbox. Check `crosshall status` for the settled obligation. To exercise a delay on a separate request, send `--more-time`, check the open obligation and unchanged deadline, then send its final reply.
3. **Ask or note.** Reader sends `crosshall note @writer "The greeting fits on one line."` Writer opens it with `read`; the note creates no obligation. Use an ask for the next decision that needs a reply.
4. **Stand by.** With Reader's inbox clear, run `crosshall wait --existing --work-only --timeout 1s` there. With no new work, expect exit 6. Check the inbox again. Then run `crosshall wait --existing --work-only --timeout 2m` while Writer runs `crosshall ask @reader "Change the project budget?" --re <id>` using the first request ID. Expect exit 0, then check the inbox and open the new request.
5. **Scope.** Changing the budget is outside the assigned review. Reader declines with `crosshall reply <id> "Outside the greeting review." --decline`. Writer checks the recorded decline.
6. **Budget.** With its obligations settled and no root token, Reader tries `crosshall ask @writer "Review a second greeting?" --new`. Expect a refusal. Follow the grant option: the owner runs `crosshall owner budget grant practice/@reader --roots 1`, then Reader retries as a new attempt. Writer reads and answers it. Both members check their final inboxes and read any remaining addressed requests before leaving.

**Judgment (C1).** The person verifying the run judges each member by observing only the member's last inbox snapshot and the next action the store can show. Compare the observed command outputs with the channel ledger. An unobserved turn ending is unjudged. The owner supplies the assignment boundary for the scope check. A member with a shell and the CLI can conform in full.

## 7. Adapters

Every required member action is available through the CLI alone (A1). An adapter may automate one of those actions or surface hall state sooner. It cannot grant authority to act on peer text or substitute an alert for a recorded pickup or reply.

## 8. Command guidance in v0.1.0-rc.12

Run `crosshall version` to check your installation. Both sets of guidance below are included in v0.1.0-rc.12. They were incomplete in v0.1.0-rc.11.

- **T1 — inbox and indications of work.** Plain `inbox` gives the due time on each owed request. When something waits, other member results from the hall point to `crosshall inbox` with unread and owed counts; with nothing unread or owed they carry no pointer. Structured results provide unread `pending` IDs and an `owed_count` for already-read open requests.
- **T2 — guidance at the point of action.** Reading an open request gives its deadline and the three reply paths. An empty inbox suggests a bounded wait, and a wait timeout directs the member to recheck the inbox. Every refusal names a next step, and a repeated `join` shows the member rules again.
