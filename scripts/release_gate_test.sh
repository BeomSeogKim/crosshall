#!/usr/bin/env bash
# Checks of scripts/release-gate.sh that need no build: it never removes the
# caller's files, the cross-vendor row passes only when both directions do,
# and the store row allows only the changes a migration statement selects.
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

# 2. The cross-vendor verdict.
judge() { # judge <want-exit> < events
  local want=$1 got=0 said
  said=$(python3 -B "$here/release_gate_events.py" T claude_to_codex codex_to_claude) || got=$?
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

# 3. The upgrade comparison allows only what a migration statement selects.
python3 -B - "$here/release_gate_rows.py" "$root" <<'PY'
import sqlite3, subprocess, sys
rows, root = sys.argv[1], sys.argv[2]
migration = root + "/002_cutover.sql"
with open(migration, "w") as f:
    f.write("UPDATE m SET body = 'reset'\nWHERE kind = 'ask';\nDELETE FROM p WHERE level > 1;\n"
            "CREATE TRIGGER t AFTER INSERT ON m BEGIN\nUPDATE k SET v = 0;\nEND;\nINSERT INTO k (v) VALUES (9);\n")
def store(name, *changes):
    path = f"{root}/{name}.sqlite3"
    db = sqlite3.connect(path)
    db.executescript("CREATE TABLE m (id INTEGER PRIMARY KEY, kind TEXT, body TEXT);"
                     "CREATE TABLE p (a INTEGER, b INTEGER, level INTEGER, PRIMARY KEY (a, b)) WITHOUT ROWID;"
                     "CREATE TABLE k (v INTEGER); CREATE TABLE u (v INTEGER);"
                     "INSERT INTO m VALUES (1, 'ask', 'q'), (2, 'note', 'n');"
                     "INSERT INTO p VALUES (1, 1, 1), (1, 2, 2); INSERT INTO k VALUES (1); INSERT INTO u VALUES (5);")
    for change in changes:
        db.executescript(change)
    db.commit()
    return path
before = store("before")
cutover = ("UPDATE m SET body = 'reset' WHERE kind = 'ask'; DELETE FROM p WHERE level > 1;"
           "INSERT INTO k (v) VALUES (9);")
cases = [
    ("the migration's own changes", 0, store("ok", cutover)),
    ("an unselected deletion", 1, store("gone", cutover, "DELETE FROM m WHERE id = 2;")),
    ("an unselected change", 1, store("note", cutover, "UPDATE m SET body = 'x' WHERE id = 2;")),
    ("a change only a trigger body names", 1, store("trigger", cutover, "UPDATE k SET v = 0;")),
    ("a lost row of an untouched table", 1, store("plain", cutover, "DELETE FROM u;")),
]
for label, want, after in cases:
    got = subprocess.run([sys.executable, "-B", rows, before, after, migration], capture_output=True, text=True)
    if got.returncode != want:
        sys.exit(f"release_gate_test: {label}: exit {got.returncode}, want {want}\n{got.stdout}{got.stderr}")
PY

echo "release gate checks passed"
