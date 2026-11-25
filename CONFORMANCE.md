# Member conformance results

Each row records an exercise of the member rules in a new hall. The Reader and Writer used the Crosshall CLI without hooks. Bash ran a scripted reference case through a persistent `crosshall run` shell. The named agents ran the same exercise from their own shell tools. The driver checked their session identity with `whoami`, and each agent supplied three choices about the assigned work. The commands for both participants were scripted, and participants could see the driver. These results establish that the displayed versions completed an assisted exercise. An unobserved turn ending, independent adoption of the rules, and future sessions are unjudged. [Member protocol](PROTOCOL.md)

<!-- generated member runs -->
| Reader | Reader version | Run | Crosshall build | Host | R1 | R2 | R3 | R4 | R5 | R6 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Bash | 3.2.57(1)-release | scripted shell | git-730db4f | macOS | pass | pass | pass | pass | pass | previous rule |
| Codex | 0.170.0 | assisted session | git-730db4f | macOS | pass | pass | pass | pass | pass | previous rule |
| Claude Code | 2.1.328 | assisted session | git-730db4f | macOS | pass | pass | pass | pass | pass | previous rule |
| Antigravity | 1.5.11 | assisted session | git-730db4f | macOS | pass | pass | pass | pass | pass | previous rule |
| Bash | 3.2.57(1)-release | scripted shell | git-dcce9f1 | macOS | pass | pass | pass | pass | pass | pass |
| Codex | 0.171.0 | assisted session | git-dcce9f1 | macOS | pass | pass | pass | pass | pass | pass |
| Claude Code | 2.1.330 | assisted session | git-dcce9f1 | macOS | pass | incomplete | incomplete | incomplete | incomplete | incomplete |
| Claude Code | 2.1.330 | assisted session | git-dcce9f1 | macOS | pass | pass | pass | pass | pass | pass |
| Antigravity | 1.5.14 | assisted session | git-dcce9f1 | macOS | pass | pass | pass | pass | pass | pass |
<!-- end member runs -->

The rows marked `previous rule` preserve the earlier R6 result on `730db4f`. The current R6 checks how the reader responds to a loop guard stop. The first Claude Code run on `dcce9f1` stopped at R2. A second run supplied the member rule and made clear that the review was still pending at its deadline.

R1 through R6 follow the order of the member rules. Each pass requires the listed observations in one run. An incomplete column means the run stopped before that check passed. The receipts contain the date, observed identity type and completed checks. They omit session identifiers and machine paths. [Run receipts](test/conformance/member-runs.jsonl)

| Rule | What the driver checks |
| --- | --- |
| R1, check the inbox | The addressed request appears in an inbox snapshot. The Reader opens that ID, sees the Writer as sender, and the Writer's ledger records pickup. Both participants check their final inboxes. |
| R2, answer by the deadline | A final answer settles the first request on time. For a second review still in progress, the Reader chooses how to report the delay. A more-time reply keeps the obligation open at its original deadline. The final reply references that request and settles it on time. |
| R3, ask or note | A question creates an obligation. The Writer reads the Reader's informational note, and the ledger records no obligation for the note. |
| R4, wait when clear | A bounded wait after an empty inbox exits 6. The Reader checks again and waits. A request sent during the second wait returns as a new arrival, still unread. The Reader checks its inbox and opens the request. |
| R5, stay within the assignment | The owner assigns a greeting review. When the Writer requests a project budget change, the Reader chooses its response. The ledger records a decline. |
| R6, respond to a loop guard stop | The owner sets the Reader's new-thread limit to zero. A new ask exits 3 with `round_limit`. The Reader notes what it has. After the owner resets the limit to eight, the Reader sends a separate request and the Writer answers it. |

The driver judges observable action in a live run. It does not simulate an agent returning after an actual offline deadline, a vendor automatically waking up, or a vendor hook stopping a turn. Hall-level regression tests separately cover late replies, per-recipient cancellation, atomic fan-out, same-key retry and fail-open hooks. [Hall-level checks](#hall-level-checks)

## Reproduce a row

Use Python 3.8 or later and Bash on macOS or Linux. For the rows above, use the binary and driver from the recorded Crosshall revision. Run these commands from a checkout that contains that revision.

```sh
fixture=$(mktemp -d)
mkdir "$fixture/source"
revision=$(git rev-parse --short HEAD)
git archive "$revision" | tar -x -C "$fixture/source"
go -C "$fixture/source" build -trimpath -ldflags "-X main.version=git-$revision" \
  -o "$fixture/crosshall" ./cmd/crosshall
python3 "$fixture/source/scripts/protocol-conformance.py" run --crosshall "$fixture/crosshall" --reader shell
```

The last command reproduces the Bash row. To run a vendor row, make the driver and test binary available to the running agent. Assign it the sample greeting review. It must report pending work by the deadline. The Reader reports what it has after a round refusal. The owner can reset the limit and assign a new review. Ask it to run the following through its shell tool and answer the three prompts. Use `claude`, `codex` or `agy` for the Reader and replace the version with the public version of that same client. A session that cannot identify its version can report `unknown`. The three native participants identified their client versions with `claude --version`, `codex --version` and `agy --version`. Antigravity used its standard sandbox. Crosshall independently checks the identity type, while the version label comes from the caller.

```sh
python3 "$fixture/source/scripts/protocol-conformance.py" run --crosshall "$fixture/crosshall" \
  --reader codex --agent-version 0.170.0
```

A native shell tool without interactive stdin can supply the agent's three answers on separate input lines in the order printed. For an agent without a Crosshall identity adapter, start it with `crosshall run -- <agent>` and use `--reader route --agent-name Example --agent-version <version>` in its own shell tool. The exercise checks the inherited route. A run using manufactured vendor variables cannot supply evidence about an actual vendor session.

Every invocation makes a fresh home, state directory, dispatcher and Writer. Only the Reader inherits the calling session's identity. The driver stops the dispatcher it started and removes its private hall on exit. Reproduction needs no vendor account for the scripted shell case. A new run writes one JSON receipt to stdout. Prompts and diagnostics go to stderr. Keep the exit status with the receipt.

To publish a run, append its receipt to `test/conformance/member-runs.jsonl`. Render the table with `python3 scripts/protocol-conformance.py table`, copy it into the marked block above and verify it with `python3 scripts/protocol-conformance.py table --check`. Each pass is derived from that rule's expected evidence. An unexpected evidence value fails validation.

## Claude Code hook manifest

The hook reads the session's build and mode from the environment Claude Code gives its hooks, and only labels in an observed form count; anything else is `unverified`, and evidence cannot mark an unverified profile tested. Manifests `claude-2` and `claude-3` accept these observations:

| Session | `AI_AGENT` | `CLAUDE_CODE_ENTRYPOINT` | Profile |
| --- | --- | --- | --- |
| `claude -p` (2.1.327) | `claude-code_2-1-327_harness` | `sdk-cli` | `2.1.327`, `print` |
| interactive, a person's session (2.1.329, macOS) | `claude-code_2-1-329_agent` | `cli` | `2.1.329`, `interactive` |
| interactive, the conformance run's scratch home (2.1.330, macOS) | `claude-code_2-1-330_harness` | `cli` | `2.1.330`, `interactive` |

The last part of `AI_AGENT` is not the mode: both forms were seen in interactive sessions. The build comes from `AI_AGENT` and the mode from `CLAUDE_CODE_ENTRYPOINT` alone. `claude-1` accepted only the first row, so an interactive session was unverified. The profile carries no permission mode: tested evidence for an interactive build covers its permission modes except bypass, whose sessions never receive notices.

### The interactive run

`go test -tags conformance -run '^TestClaudeCodeInteractive' -v ./test/conformance/` drives a real interactive session on a pseudo-terminal, in temporary homes against the local stand-in for the model, with the adapter installed. Its scratch configuration marks the first-run steps done, so the session starts without a dialog; the run sets neither the build nor the mode. On Claude Code 2.1.330 (macOS arm64) it observed:

- `SessionStart` with `startup`, `resume`, `compact` and `clear`; `UserPromptSubmit` and `Stop` for every turn; `Notification` with `permission_prompt` when an approval dialog opens, and the approved turn goes on to its `Stop`.
- `/compact` keeps the session ID. `/clear` ends the session (`SessionEnd`, reason `clear`) and starts one with a new ID in the same process, which stays the same member. `/exit` ends it with reason `prompt_input_exit`, the member departs, and resuming the session restores it.
- A prompt typed at an idle prompt opens a turn of its own: its `UserPromptSubmit` and its `Stop` carry one prompt ID that no other turn has.
- A prompt typed ahead, while the turn before it is still running, is reported with that earlier turn's prompt ID, before the earlier turn's `Stop`; its own turn then ends with a `Stop` whose ID no prompt carried. The adapter takes such a prompt as the start of a turn of its own.
- A notice the hall publishes while a turn is with the model does not take that shape: it runs after the turn's `Stop` as a turn of its own, with its own prompt ID.
- With tested evidence for the build, a request stored during a turn continues that turn once with the pending line, as in print mode, also when the turn's prompt was typed ahead.

## Tested capability evidence

Tested evidence covers sessions with the same vendor, platform, mode, capability and hook manifest revision. Evidence for the exact running version takes precedence. An unverified version or mode stays ineligible. For Claude Code, the manifest revision (`claude-3` since the idle notification hook) identifies the vendor's hook callback format, profile labels and installed hooks. A manifest revision change needs evidence for the new revision.

The binary ships `turn_end` evidence for Claude Code on macOS arm64 in print and interactive mode: 2.1.330 with manifest `claude-2`, and 2.1.331 with manifest `claude-3`. The corresponding native conformance tests use a local stand-in for the model. They verify that an unread request continues an eligible turn once with the pending line.

## Publish profiles

The binary ships `publish` evidence for Claude Code 2.1.330 on macOS arm64 in interactive mode with manifest `claude-2`. The evidence comes from a witnessed run on `c1db3a4` that sent notices through the product. The run exercised all of these:

- an idle session starts one turn of its own for a notice, and the request is read;
- text typed but not sent stays as it was, and nothing is submitted;
- an open approval dialog stays open, and the notice runs after it is answered, with approval and denial both exercised;
- a notice sent while the person's turn runs comes after that turn, and the prompt and its answer are unchanged;
- after `/clear` the next notice reaches the new conversation; a resumed session in a new process is reached at its new address and the old one is never written to;
- a session that was closed, and one that was killed, receive nothing and start nothing.

It also ships `publish` evidence for Claude Code 2.1.331 on macOS arm64 in interactive mode with manifest `claude-3`, from a witnessed run of the same cases on `0132580`. Since a notice is held back while the session's turn is open, the turn-end check offered the request at the turn's Stop in the approval and busy cases, and the session read it in that turn. Those two cases were therefore run again with the turn-end check off: the notice was held while the dialog was open or the tool ran, and was delivered once after the turn's Stop, as a turn of its own. This run does not show what a notice does when it lands in an open dialog or a running turn, which can still happen when the hall's picture of the turn is stale; the `claude-2` run on `c1db3a4` is the evidence for that.

A run in print mode with a stand-in model shows only that the message line still parses; it cannot list a profile. Bypass permission mode is never covered: such a session stays on pull.

For a version covered by another version's evidence, the first three publish notices observed as their own turns confirm that build on the machine. An absorbed or unknown notice during that period stops publish for the build. Later failures suspend the current launch. The owner can lift a build stop with `crosshall owner publish resume <binding-id>` or with newer tested evidence for that exact version. `crosshall owner doctor` reports this state, and uncovered sessions show `publish: candidate` in `crosshall who`.

## Claude Code ↔ Codex round trip

With both real CLIs on `PATH`, run this release-gate check from the checkout:

```sh
go test -tags conformance -count=1 -timeout 3m -run '^TestCrossVendorRoundTrip$' -v ./test/conformance/
```

The check uses separate temporary vendor homes, one disposable dispatcher and
loopback model endpoints. It requires macOS with `sandbox-exec` or a disposable
Linux account in a network namespace with only `lo` and no real vendor homes.
To check an already-built release candidate, set
`CROSSHALL_CONFORMANCE_CROSSHALL` to its absolute path outside vendor homes.

Claude Code print mode asks first. A neutral prompt in the open Codex app-server
thread must receive the request ID in developer context before Codex reads and
answers. Codex then asks while Claude Code has a running member wait. In each direction the asker reads
the correlated answer, and the ledger verifies native session identity, pickup
and settlement.

### Native notices in both directions

Run the native release-gate check on macOS arm64 with Claude Code on `PATH`
and an unpacked official Codex managed app-server release package:

```sh
CROSSHALL_CONFORMANCE_CODEX_PACKAGE=/absolute/path/to/codex-release \
  go test -tags conformance -count=1 -timeout 3m -run '^TestCrossVendorNativeRoundTrip$' -v ./test/conformance/
```

The check uses the same candidate binary for both adapters. It starts a Claude
Code interactive session and a loaded thread on the real Codex managed server
in separate scratch homes. Both models use loopback endpoints. The fixture
uses shipped publish evidence for the two builds where present and imports
scratch evidence for either build that still needs it. Both native routes
must activate.

The temporary homes share a short root for local socket paths. A watchdog
owns that root and closes it to new hook launches when the test process
exits. It stops the processes tagged for that run and removes its scratch
files, including after a test timeout or SIGKILL.

Each direction requires a native request notice, recipient pickup and a
correlated answer. A native reply notice tells the original sender to read
the answer. Both notices must be recorded as submitted and observed in their
own turns. The fixture submits the initiating ask turn and observes the
receiving turns without entering any further prompts. The release gate has a
separate row for this scenario and requires both directions. A missing package
or unsupported host blocks that row.

### Codex daemon publish evidence

Use the same managed package setup and select `TestCodexDaemonPublishTrial`
to reproduce the publish trial for Codex `0.172.0`, `darwin-arm64`, `daemon`,
and `codex-1`:

```sh
CROSSHALL_CONFORMANCE_CODEX_PACKAGE=/absolute/path/to/codex-0.172.0-release \
  go test -tags conformance -count=1 -timeout 3m -run '^TestCodexDaemonPublishTrial$' -v ./test/conformance/
```

A Claude Code ask must queue on the idle Codex thread. The resulting new
turn reads the ask, replies, and records a submitted `own_turn` receipt.
The asker reads the reply. The app server must show exactly one new turn
and an empty queue after settlement. With a real approval open on that
thread, a second Claude ask must report the approval wait and stay unread
without creating a notice or opening another model turn. The trial leaves
the approval unanswered. Both model endpoints are on loopback. The pass
line records the observed profile, test time, evidence source and request
counts.

## Hall-level checks

The following tests use disposable state to exercise hall behavior that applies across Readers. The member table reports the observed member scenario only.

```sh
go test -count=1 -run 'Test(AskFansOutAtomically|OwnerCancelSettlesOnlyOpenRecipientsAndReplays|LateReplyStaysVisible)$' ./internal/domain
go test -tags=e2e -count=1 -run 'Test(LostAskResponseDebitsOnce|AliasInsideTheTextReachesOnlyTheRecipient|HookEntrySilentFailures|HookEntryOpenStdinBounded)$' ./test/e2e
```
