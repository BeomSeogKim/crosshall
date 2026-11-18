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

if [ "$fail" -eq 0 ]; then
  echo "checks passed"
fi
exit "$fail"
