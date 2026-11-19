#!/usr/bin/env bash
# Emit TOKEN_SAMPLE rows from model requests in disposable loopback sessions.
# Each row contains the original text for offline token estimates.
set -euo pipefail
cd "$(dirname "$0")/.."
for vendor in claude codex; do
  command -v "$vendor" >/dev/null || { echo "$vendor is required" >&2; exit 1; }
done
unset CROSSHALL_CONFORMANCE_OUT
export CROSSHALL_MEASURE_TEXT=1
exec go test -tags conformance -count=1 -v \
  -run '^Test(ClaudeCodePrintModeConformance|Claude(Presence|Reminder)TokenCost|CodexExecConformance|CodexReminderTransport)$' \
  ./test/conformance
