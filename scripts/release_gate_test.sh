#!/usr/bin/env bash
# Checks of scripts/release-gate.sh that need no build: it never removes the
# caller's files, the cross-vendor row passes only when both directions do,
# and the store row fails when any command it runs fails.
set -euo pipefail
export LC_ALL=C

here=$(cd -- "$(dirname -- "$0")" && pwd -P)
root=$(mktemp -d "${TMPDIR:-/tmp}/release-gate-test.XXXXXXXX")
trap 'rm -rf -- "$root"' EXIT
fail() {
  printf 'release_gate_test: %s\n' "$*" >&2
  exit 1
}

# 1. An output directory that already holds the store and other files. The
#    candidate has no go.mod, so the build row fails before any build.
repo=$root/repo
git init -q "$repo"
mkdir "$repo/scripts"
cp -- "$here/release-gate.sh" "$here/release_gate_open.go" "$here/release_gate_events.py" "$repo/scripts/"
commit() { git -C "$repo" -c user.name=gate -c user.email=gate@example.invalid commit -q "$@"; }
commit --allow-empty -m base
git -C "$repo" tag v0.0.1
git -C "$repo" add scripts
commit -m candidate
out=$root/out
mkdir -p "$out/work"
for f in keep work/keep work/store.sqlite3 work/store.sqlite3-wal; do
  printf '%s\n' "$f" > "$out/$f"
done
status=0
PATH="$(dirname -- "$(command -v git)"):$(dirname -- "$(command -v python3)"):/usr/bin:/bin" \
  bash "$repo/scripts/release-gate.sh" --version v0.0.2 --out "$out" --store "$out/work/store.sqlite3" \
  --skip-full-run > "$root/gate.out" 2>&1 || status=$?
[[ $status == 1 ]] || fail "gate exit $status, want 1: $(cat "$root/gate.out")"
for f in keep work/keep work/store.sqlite3 work/store.sqlite3-wal; do
  [[ $(cat "$out/$f" 2> /dev/null) == "$f" ]] || fail "the gate changed or removed $f under --out"
done
grep -q '^| Store snapshot upgrade and rollback | BLOCKED |' "$out/summary.md" ||
  fail "the store row did not wait for the builds: $(cat "$out/summary.md")"
grep -q '^| Two devices on one machine against the base | BLOCKED |' "$out/summary.md" ||
  fail "the two-device row did not wait for the builds"

# 2. The cross-vendor verdict.
judge() { # judge <want-exit> [--strict] [subtest...] < events
  local want=$1 got=0 said
  shift
  case ${1:-} in
    --strict) shift; set -- --strict T "$@" ;;
    '') set -- T claude_to_codex codex_to_claude ;;
    *) set -- T "$@" ;;
  esac
  said=$(python3 -B "$here/release_gate_events.py" "$@") || got=$?
  [[ $got == "$want" ]] || fail "verdict exit $got, want $want: $said"
}
ev() { # ev <action> <test> [output]
  printf '{"Action":"%s","Test":"%s","Output":"%s"}\n' "$1" "$2" "${3:-}"
}
{
  ev pass T/claude_to_codex_next_prompt
  ev pass T/codex_to_claude_running_wait
  ev pass T
} | judge 0
{
  ev pass T/claude_to_codex_next_prompt
  ev output T/codex_to_claude_running_wait '    x_test.go:9: no codex binary'
  ev skip T/codex_to_claude_running_wait
  ev pass T
} | judge 2
{
  ev fail T/claude_to_codex_next_prompt
  ev skip T/codex_to_claude_running_wait
  ev fail T
} | judge 1
{
  ev output T '    x_test.go:9: the gate needs codex'
  ev skip T
} | judge 2
{
  ev pass T/claude_to_codex_next_prompt
  ev pass T
} | judge 1
printf '{"Action":"output","Output":"build failed"}\n' | judge 1
{
  ev pass T/claude_to_codex_next_prompt
  ev pass T/codex_to_claude_running_wait/setup
  ev skip T/codex_to_claude_running_wait
  ev pass T
} | judge 2

{
  ev pass T/claude_to_codex_notice
  ev pass T/codex_to_claude_notice
  ev pass T
} | judge 0 claude_to_codex_notice codex_to_claude_notice
{
  ev pass T/claude_to_codex_next_prompt
  ev pass T/codex_to_claude_running_wait
  ev pass T
} | judge 1 claude_to_codex_notice codex_to_claude_notice

{
  ev pass T/head-head
  ev pass T/base-home
  ev pass T/base-spoke
  ev pass T
} | judge 0 --strict head-head base-home base-spoke
{
  ev pass T/head-head
  ev pass T/base-home
  ev pass T/base-spoke-other
  ev pass T
} | judge 1 --strict head-head base-home base-spoke
{
  ev pass T/head-head
  ev pass T/base-home
  ev skip T/base-spoke
  ev pass T
} | judge 1 --strict head-head base-home base-spoke
{
  ev skip T/out-of-window/setup
  ev pass T/out-of-window
  ev pass T
} | judge 1 --strict out-of-window

# 3. The store row fails when any command it runs fails. A shim makes the
#    n-th call of sqlite3, cp, chmod, mkdir, find, cmp or join exit 7, for
#    every n, on an upgrade with a migration and on one without.
if ! sqlite=$(command -v sqlite3); then
  fail "sqlite3 is needed for the store row checks"
fi
functions=$(sed -n -e '/^run() {/,/^}/p' -e '/^capture() {/,/^}/p' -e '/^capture_some() {/,/^}/p' \
  -e '/^lines() {/,/^}/p' -e '/^store_sound() {/,/^}/p' -e '/^same_schema() {/,/^}/p' \
  -e '/^copy_store() {/,/^}/p' -e '/^row_counts() {/,/^}/p' -e '/^row_store() {/,/^}/p' "$here/release-gate.sh")
[[ $(grep -c '^[a-z_]*() {' <<< "$functions") == 9 ]] || fail "the store row functions were not found"
eval "$functions"
needs_builds() { :; }
detail() { :; }
# The production opener's part: migrate schema 1 to 2 with a backup, create
# schema 2 when empty, and leave the base's own schema alone.
open_store() {
  local db=$2/store.sqlite3 version
  [[ $1 == base ]] && return 0
  if [[ ! -e $db ]]; then
    "$sqlite" "$db" 'CREATE TABLE m (id INTEGER PRIMARY KEY, body TEXT); CREATE TABLE n (v INTEGER); PRAGMA user_version = 2;'
    return
  fi
  version=$("$sqlite" "$db" 'PRAGMA user_version')
  if [[ $version == 1 ]]; then
    "$sqlite" "$db" ".backup '$2/store-v1-gate.sqlite3'" &&
      "$sqlite" "$db" 'CREATE TABLE n (v INTEGER); PRAGMA user_version = 2;'
  fi
}
shims=$root/shims
mkdir "$shims"
for tool in sqlite3 cp chmod mkdir find cmp join; do
  real=$(command -v "$tool") || fail "$tool is missing"
  printf '#!/bin/sh\nn=$(($(cat "$GATE_CALLS") + 1))\necho "$n" > "$GATE_CALLS"\n[ "$n" = "$GATE_FAIL_AT" ] && exit 7\nexec %s "$@"\n' \
    "$real" > "$shims/$tool"
  chmod 700 "$shims/$tool"
done
# shellcheck disable=SC2034 # globals the store row reads
head_tree=head base_tree=base base=base
for case in migration same; do
  live_store=$root/live-$case.sqlite3
  "$sqlite" "$live_store" "CREATE TABLE m (id INTEGER PRIMARY KEY, body TEXT); INSERT INTO m VALUES (1, 'a');"
  if [[ $case == migration ]]; then
    "$sqlite" "$live_store" 'PRAGMA user_version = 1'
    # shellcheck disable=SC2034
    base_schema=1
  else
    "$sqlite" "$live_store" 'CREATE TABLE n (v INTEGER); PRAGMA user_version = 2'
    # shellcheck disable=SC2034
    base_schema=2
  fi
  # shellcheck disable=SC2034
  head_schema=2
  calls=0
  at=0
  while :; do
    work=$root/$case-$at
    mkdir "$work"
    echo 0 > "$root/calls"
    status=0
    GATE_CALLS=$root/calls GATE_FAIL_AT=$at PATH="$shims:$PATH" row_store > "$work.log" 2>&1 || status=$?
    if ((at == 0)); then
      [[ $status == 0 ]] || fail "the $case store row failed without a fault: $(cat "$work.log")"
      calls=$(cat "$root/calls")
    else
      [[ $status == 1 ]] || fail "the $case store row returned $status when call $at of $calls failed"
    fi
    ((++at <= calls)) || break
  done
done

# 4. The full-run row's neighbours: a process counts as left by an earlier
#    row only when its executable is one the gate built; a mere path in the
#    command line is named, not blamed; test runs are told by their argv.
#    The gate's directories hold a space, as a user's --out path may.
w="/gate/the work" o="/gate/the out"
classes=$(awk -v work="$w" -v out="$o" -f "$here/release_gate_others.awk" <<PS
  101 00:01 $o/bin/crosshall __dispatch
  102 00:01 $w/crosshall-link-base __dispatch
  103 00:01 tail -f $o/summary.md
  104 00:01 git diff -- scripts/ci-checks.sh
  105 00:01 /opt/homebrew/bin/go test ./internal/dispatch
  106 00:01 /var/folders/x/go-build1/b001/e2e.test -test.paniconexit0 -test.run=Link
  107 00:01 bash scripts/ci-checks.sh
  108 00:01 /usr/bin/vim notes-on-go-test.txt
  109 00:01 /bin/zsh -c echo $o/bin/crosshall
PS
)
want="gate: 101
gate: 102
names: 103
test: 105
test: 106
test: 107
names: 109"
[[ $(awk '{print $1, $2}' <<< "$classes") == "$want" ]] || fail "process classes: $classes"

echo "release gate checks passed"
