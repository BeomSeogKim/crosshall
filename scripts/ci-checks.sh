#!/usr/bin/env bash
# Repository checks. CI runs this; run it locally before opening a pull request.
set -euo pipefail

fail=0

# 1. Local working notes must never be tracked.
if [ -n "$(git ls-files -- .notes)" ]; then
  echo "error: files under .notes/ are tracked; they must stay local" >&2
  fail=1
fi

# 2. Community files that the project promises must exist.
for f in LICENSE NOTICE README.md CONTRIBUTING.md CODE_OF_CONDUCT.md SECURITY.md AGENTS.md; do
  if [ ! -f "$f" ]; then
    echo "error: missing $f" >&2
    fail=1
  fi
done

# 3. On a pull request the title becomes the squash commit, so it must be a
#    Conventional Commit. PR_TITLE is set by the workflow; unset locally.
if [ -n "${PR_TITLE:-}" ]; then
  re='^(feat|fix|docs|chore|refactor|test|ci|build|perf|revert)(\([a-z0-9._-]+\))?!?: .+'
  if ! [[ "$PR_TITLE" =~ $re ]]; then
    echo "error: pull request title is not a Conventional Commit: $PR_TITLE" >&2
    fail=1
  fi
fi

# 4. No AI attribution trailers or footers in commit messages or the PR body
#    (the PR body becomes the squash commit message). PR_BODY is set by the
#    workflow; commit messages are whatever history this checkout contains.
ai_re='(Co-Authored-By:.*(Claude|Codex|OpenAI|Anthropic|GPT|Gemini|noreply@anthropic\.com))|Claude-Session:|Codex-Session:|Generated with \[?(Claude|Codex)'
if git log --format=%B 2>/dev/null | grep -Eiq "$ai_re"; then
  echo "error: a commit message contains an AI attribution line" >&2
  fail=1
fi
if [ -n "${PR_BODY:-}" ] && printf '%s' "$PR_BODY" | grep -Eiq "$ai_re"; then
  echo "error: the pull request body contains an AI attribution line" >&2
  fail=1
fi

# Keep the caller's Go caches, whose defaults depend on the original home.
go_build_cache=$(go env GOCACHE)
go_module_cache=$(go env GOMODCACHE)
export GOCACHE="$go_build_cache" GOMODCACHE="$go_module_cache"

test_root=$(mktemp -d /tmp/crosshall-ci.XXXXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
test_root=$(cd -- "$test_root" && pwd -P)
export CROSSHALL_TEST_VENDOR_SENTINEL="$test_root/sentinel"
mkdir -m 700 -- "$CROSSHALL_TEST_VENDOR_SENTINEL"
# Record names only. The sentinel never opens the caller's configuration.
{
  if [[ ${HOME:-} = /* ]]; then
    printf '%s\n' "$HOME/.claude" "$HOME/.codex"
    if physical_home=$(cd -- "$HOME" && pwd -P); then
      printf '%s\n' "$physical_home/.claude" "$physical_home/.codex"
    fi
  fi
  for dir in "${CLAUDE_CONFIG_DIR:-}" "${CODEX_HOME:-}"; do
    if [[ $dir = /* ]]; then
      printf '%s\n' "$dir"
    fi
  done
} > "$CROSSHALL_TEST_VENDOR_SENTINEL/protected"

export HOME="$test_root/home" TMPDIR="$test_root/tmp"
export CLAUDE_CONFIG_DIR="$test_root/claude" CODEX_HOME="$test_root/codex"
export XDG_CONFIG_HOME="$test_root/xdg-config" XDG_CACHE_HOME="$test_root/xdg-cache"
export XDG_DATA_HOME="$test_root/xdg-data" XDG_STATE_HOME="$test_root/xdg-state"
mkdir -m 700 -- "$HOME" "$TMPDIR" "$CLAUDE_CONFIG_DIR" "$CODEX_HOME" \
  "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME"
# Test binaries built by a Go subprocess need the same sentinel tag.
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-tags=testvendorhome"

# 5. Go formatting, static analysis and tests.
if unformatted="$(gofmt -l .)"; then
  if [ -n "$unformatted" ]; then
    printf 'error: run gofmt on:\n%s\n' "$unformatted" >&2
    fail=1
  fi
else
  fail=1
fi
if ! go vet ./...; then
  fail=1
fi
if ! go test -count=1 ./...; then
  fail=1
fi
# End-to-end checks run the built binary with its dispatcher.
if ! go vet -tags e2e,testvendorhome ./test/e2e/ || ! go test -tags e2e,testvendorhome -count=1 ./test/e2e/; then
  fail=1
fi
if ! bash scripts/install_test.sh; then
  fail=1
fi
if ! go test -tags installer,testvendorhome -count=1 ./test/installer/; then
  fail=1
fi
if [ -s "$CROSSHALL_TEST_VENDOR_SENTINEL/violations" ]; then
  cat "$CROSSHALL_TEST_VENDOR_SENTINEL/violations" >&2
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "checks passed"
fi
exit "$fail"
