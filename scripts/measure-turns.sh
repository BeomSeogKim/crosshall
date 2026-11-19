#!/usr/bin/env bash
# Usage: scripts/measure-turns.sh [pairs for the shell loop] [claude|codex]
# Measures running-session prompts, startup and teardown separately, then
# records the whole resumed invocation with zero or ten sequential shell calls.
# The post-probe arm measures a fixture-only PostToolUse matcher and the
# startup/refusal of an unknown hook in the supplied crosshall binary.
# Each CLI needs to be on PATH. Linux requires a loopback-only network
# namespace and a disposable account without vendor homes mounted.
set -euo pipefail
cd "$(dirname "$0")/.."
case "${2:-}" in
  '') quiet='^Test(ClaudeRunningTurnLatency|CodexSessionLatency)$'; loop='^TestPairedTurnLatency$' ;;
  claude) quiet='^TestClaudeRunningTurnLatency$'; loop='^TestPairedTurnLatency$/^claude$' ;;
  codex) quiet='^TestCodexSessionLatency$'; loop='^TestPairedTurnLatency$/^codex$' ;;
  *) echo 'expected claude or codex' >&2; exit 2 ;;
esac
export CROSSHALL_TURN_PAIRS="${1:-20}"
export CROSSHALL_CLAUDE_LIVE_PAIRS="${CROSSHALL_CLAUDE_LIVE_PAIRS:-100}"
export CROSSHALL_SESSION_PAIRS="${CROSSHALL_SESSION_PAIRS:-100}"
go test -tags conformance -count=1 -timeout=2h -v -run "$quiet" ./test/conformance
exec go test -tags conformance -count=1 -timeout=2h -v -run "$loop" ./test/conformance
