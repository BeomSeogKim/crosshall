# Member conformance results

Each row exercised the six member rules in a new hall. The Reader and Writer used the Crosshall CLI without hooks. Bash ran a scripted reference case through a persistent `crosshall run` shell. The named agents ran the same exercise from their own shell tools. The driver checked their session identity with `whoami`, and each agent supplied three choices about the assigned work. The commands for both participants were scripted, and participants could see the driver. These results establish that the displayed versions completed an assisted exercise. An unobserved turn ending, independent adoption of the rules, and future sessions are unjudged. [Member protocol](PROTOCOL.md)

<!-- generated member runs -->
| Reader | Reader version | Run | Crosshall build | Host | R1 | R2 | R3 | R4 | R5 | R6 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Bash | 3.2.57(1)-release | scripted shell | git-730db4f | macOS | pass | pass | pass | pass | pass | pass |
| Codex | 0.170.0 | assisted session | git-730db4f | macOS | pass | pass | pass | pass | pass | pass |
| Claude Code | 2.1.328 | assisted session | git-730db4f | macOS | pass | pass | pass | pass | pass | pass |
| Antigravity | 1.5.11 | assisted session | git-730db4f | macOS | pass | pass | pass | pass | pass | pass |
<!-- end member runs -->

These runs used `730db4f`, tagged `v0.1.0-rc.13` during testing for v0.1.0.

R1 through R6 follow the order of the member rules. Each pass requires the listed observations in one run. An incomplete column means the run stopped before that check passed. The receipts contain the date, observed identity type and completed checks. They omit session identifiers and machine paths. [Run receipts](test/conformance/member-runs.jsonl)

| Rule | What the driver checks |
| --- | --- |
| R1, check the inbox | The addressed request appears in an inbox snapshot. The Reader opens that ID, sees the Writer as sender, and the Writer's ledger records pickup. Both participants check their final inboxes. |
| R2, answer by the deadline | A final answer settles the first request on time. For a second review still in progress, the Reader chooses how to report the delay. A more-time reply keeps the obligation open at its original deadline. The final reply references that request and settles it on time. |
| R3, ask or note | A question creates an obligation. The Writer reads the Reader's informational note, and the ledger records no obligation for the note. |
| R4, wait when clear | A bounded wait after an empty inbox exits 6. The Reader checks again and waits. A request sent during the second wait returns as a new arrival, still unread. The Reader checks its inbox and opens the request. |
| R5, stay within the assignment | The owner assigns a greeting review. When the Writer requests a project budget change, the Reader chooses its response. The ledger records a decline. |
| R6, follow a budget refusal | With no root token remaining, a new ask exits 3 and names a grant. The Reader chooses an authorized path. After the owner grants a token, a new attempt succeeds and the Writer answers it. |

The driver judges observable action in a live run. It does not simulate an agent returning after an actual offline deadline, a vendor automatically waking up, or a vendor hook stopping a turn. Hall-level regression tests separately cover late replies, per-recipient cancellation, atomic fan-out, same-key retry and fail-open hooks. [Hall-level checks](#hall-level-checks)

## Reproduce a row

Use Python 3.8 or later and Bash on macOS or Linux. For the rows above, build the recorded Crosshall revision with Go from a checkout that contains that revision. Run these commands from the checkout containing this page.

```sh
fixture=$(mktemp -d)
mkdir "$fixture/source"
git archive 730db4f | tar -x -C "$fixture/source"
go -C "$fixture/source" build -trimpath -ldflags '-X main.version=git-730db4f' \
  -o "$fixture/crosshall" ./cmd/crosshall
python3 scripts/protocol-conformance.py run --crosshall "$fixture/crosshall" --reader shell
```

The last command reproduces the Bash row. To run a vendor row, make the driver and test binary available to the running agent. Assign it the sample greeting review. It must report pending work by the deadline. The owner will fund a separately authorized greeting review on request. Ask it to run the following through its shell tool and answer the three prompts. Use `claude`, `codex` or `agy` for the Reader and replace the version with the public version of that same client. A session that cannot identify its version can report `unknown`. The three native participants identified their client versions with `claude --version`, `codex --version` and `agy --version`. Antigravity used its standard sandbox. Crosshall independently checks the identity type, while the version label comes from the caller.

```sh
python3 scripts/protocol-conformance.py run --crosshall /absolute/path/to/crosshall \
  --reader codex --agent-version 0.170.0
```

A native shell tool without interactive stdin can supply the agent's three answers on separate input lines in the order printed. For an agent without a Crosshall identity adapter, start it with `crosshall run -- <agent>` and use `--reader route --agent-name Example --agent-version <version>` in its own shell tool. The exercise checks the inherited route. A run using manufactured vendor variables cannot supply evidence about an actual vendor session.

Every invocation makes a fresh home, state directory, dispatcher and Writer. Only the Reader inherits the calling session's identity. The driver stops the dispatcher it started and removes its private hall on exit. Reproduction needs no vendor account for the scripted shell case. A new run writes one JSON receipt to stdout. Prompts and diagnostics go to stderr. Keep the exit status with the receipt.

To publish a run, append its receipt to `test/conformance/member-runs.jsonl`. Render the table with `python3 scripts/protocol-conformance.py table`, copy it into the marked block above and verify it with `python3 scripts/protocol-conformance.py table --check`. Each pass is derived from that rule's expected evidence. An unexpected evidence value fails validation.

## Hall-level checks

The following tests use disposable state to exercise hall behavior that applies across Readers. The member table reports the observed member scenario only.

```sh
go test -count=1 -run 'Test(AskFansOutAtomically|OwnerCancelSettlesOnlyOpenRecipientsAndReplays|LateReplyStaysVisible)$' ./internal/domain
go test -tags=e2e -count=1 -run 'Test(LostAskResponseDebitsOnce|AliasInsideTheTextReachesOnlyTheRecipient|HookEntrySilentFailures|HookEntryOpenStdinBounded)$' ./test/e2e
```
