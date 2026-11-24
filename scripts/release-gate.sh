#!/usr/bin/env bash
# Run the recurring rows of a release gate on a candidate commit and print a
# summary table. Rows for the release's new features stay manual; record them
# next to this summary in the release's gate report. Exit status: 0 every row
# passed, 1 a row failed, 3 a row was blocked or skipped, 2 usage.
set -uo pipefail
export LC_ALL=C

usage='Usage: scripts/release-gate.sh --version <tag> [--commit <rev>] [--base <tag>] [--store <path>] [--out <dir>] [--skip-full-run]'
die() {
  printf '%s\n' "$*" >&2
  exit 2
}

version=''
commit=HEAD
base=''
live_store=''
out=''
full_run=true
while (($#)); do
  case $1 in
    --version | --commit | --base | --store | --out)
      (($# >= 2)) || die "$usage"
      case $1 in
        --version) version=$2 ;;
        --commit) commit=$2 ;;
        --base) base=$2 ;;
        --store) live_store=$2 ;;
        --out) out=$2 ;;
      esac
      shift 2
      ;;
    --skip-full-run)
      full_run=false
      shift
      ;;
    *) die "$usage" ;;
  esac
done
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.+-]+)?$ ]] || die "$usage"

repo=$(cd -- "$(dirname -- "$0")/.." && pwd -P)
commit=$(git -C "$repo" rev-parse --verify --quiet "$commit^{commit}") || die "Unknown commit."
if [[ -z $base ]]; then
  base=$(git -C "$repo" describe --tags --abbrev=0 --match 'v*' "$commit^") || die "No earlier release tag; pass --base."
fi
base_commit=$(git -C "$repo" rev-parse --verify --quiet "refs/tags/$base^{commit}") || die "Unknown base tag $base."
if [[ -z $live_store ]]; then
  # The default state root of internal/dispatch.DefaultStateRoot.
  if [[ ${XDG_STATE_HOME:-} == /* ]]; then
    live_store=$XDG_STATE_HOME/crosshall/store.sqlite3
  else
    live_store=$HOME/.local/state/crosshall/store.sqlite3
  fi
fi
if [[ -z $out ]]; then
  out=$(mktemp -d "${TMPDIR:-/tmp}/release-gate.XXXXXXXX") || exit 2
else
  mkdir -p -m 700 -- "$out" || exit 2
fi
out=$(cd -- "$out" && pwd -P)
mkdir -p -m 700 -- "$out/bin"
# Store snapshots, worktrees and scratch homes live in a directory of their
# own, removed at exit; out keeps the logs, the binaries and the summary.
work=$(mktemp -d "${TMPDIR:-/tmp}/release-gate-work.XXXXXXXX") || exit 2
work=$(cd -- "$work" && pwd -P)
mkdir -m 700 -- "$work/tree"
: > "$work/dispatchers"

cleanup() {
  local bin state home tree
  while IFS='|' read -r bin state home; do
    env -i HOME="$home" PATH=/usr/bin:/bin CROSSHALL_STATE_DIR="$state" "$bin" owner stop > /dev/null 2>&1
  done < "$work/dispatchers"
  for tree in "$work"/tree/*; do
    [[ -d $tree ]] && git -C "$repo" worktree remove --force "$tree" > /dev/null 2>&1
  done
  rm -rf -- "$work"
}
trap cleanup EXIT

head_tree=$work/tree/head
base_tree=$work/tree/base
cand=$out/bin/crosshall
base_bin=$out/bin/crosshall-$base
built=false
base_schema=''
head_schema=''

titles=()
results=()
details=()
current=''
detail() {
  printf '%s\n' "$*" > "$out/$current.detail"
}
row() {
  local title=$1 status result
  current=$2
  : > "$out/$current.detail"
  printf '== %s\n' "$title" >&2
  "$current" > "$out/$current.log" 2>&1
  status=$?
  case $status in
    0) result=PASS ;;
    2) result=BLOCKED ;;
    3) result=SKIP ;;
    *) result=FAIL ;;
  esac
  titles+=("$title")
  results+=("$result")
  details+=("$(head -n 1 "$out/$current.detail") (log: $current.log)")
  printf '   %s\n' "$result" >&2
}
needs_builds() {
  $built && return 0
  detail "needs the candidate and base builds"
  return 2
}

# The environment of scripts/release.sh for a host-platform build.
build() { # build <source-dir> <version> <output>
  (cd -- "$1" && GOENV=off GOFLAGS='' GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=0 GOEXPERIMENT='' \
    GOAMD64=v1 GOARM64=v8.0 go build -mod=readonly -buildvcs=false -trimpath -pgo=off \
    -ldflags "-X main.version=$2 -buildid=" -o "$3" ./cmd/crosshall)
}

# A plain owner shell: no route or agent variable reaches crosshall.
hall() { # hall <state-dir> <home> [VAR=value...] <command...>
  local state=$1 home=$2
  shift 2
  env -i HOME="$home" PATH=/usr/bin:/bin TMPDIR="$home" CROSSHALL_STATE_DIR="$state" \
    CROSSHALL_TELEMETRY=off "$@"
}
started() { # started <binary> <state-dir> <home>
  printf '%s|%s|%s\n' "$1" "$2" "$3" >> "$work/dispatchers"
}
json() { # json <python expression over d> < document
  python3 -c 'import json, sys; d = json.load(sys.stdin); print(eval(sys.argv[1]))' "$1"
}

# --- Rows -------------------------------------------------------------------

row_build() {
  local want have stage
  printf 'candidate %s %s\nbase %s %s\nstarted %s\n' "$version" "$commit" "$base" "$base_commit" \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  uname -sm
  command -v sw_vers > /dev/null && sw_vers -productVersion
  go version
  for vendor in claude codex; do
    command -v "$vendor" > /dev/null && printf '%s %s\n' "$vendor" "$("$vendor" --version 2>&1 | head -n 1)"
  done
  want=$(git -C "$repo" show "$commit:go.mod" | awk '$1 == "go" { print "go" $2; exit }')
  have=$(GOTOOLCHAIN=local go env GOVERSION)
  [[ $want == "$have" ]] || { detail "release builds need $want, found $have"; return 1; }
  git -C "$repo" worktree add -q --detach "$head_tree" "$commit" &&
    git -C "$repo" worktree add -q --detach "$base_tree" "$base_commit" ||
    { detail "could not check out the candidate and base"; return 1; }
  build "$head_tree" "$version" "$cand" || { detail "candidate build failed"; return 1; }
  # A second build from another directory must give the same bytes.
  stage=$work/archive
  mkdir -- "$stage" && git -C "$repo" archive "$commit" | tar -x -C "$stage" &&
    build "$stage" "$version" "$work/crosshall-again" || { detail "second candidate build failed"; return 1; }
  cmp -- "$cand" "$work/crosshall-again" || { detail "two builds of the candidate differ"; return 1; }
  [[ $("$cand" version) == "$version" ]] || { detail "candidate reports $("$cand" version)"; return 1; }
  build "$base_tree" "$base" "$base_bin" || { detail "base build failed"; return 1; }
  [[ $("$base_bin" version) == "$base" ]] || { detail "base binary reports $("$base_bin" version)"; return 1; }
  shasum -a 256 "$cand" "$base_bin" | tee "$out/bin/SHA256SUMS"
  built=true
  detail "$version $(shasum -a 256 "$cand" | cut -c1-12), reproducible; base $base built"
}

row_source() {
  local changed line pins date
  # The attribution pattern of scripts/ci-checks.sh.
  local ai_re='(Co-Authored-By:.*(Claude|Codex|OpenAI|Anthropic|GPT|Gemini|noreply@anthropic\.com))|Claude-Session:|Codex-Session:|Generated with \[?(Claude|Codex)'
  git -C "$repo" diff --name-status "$base_commit" "$commit" > "$out/changed-paths.txt" || return 1
  changed=$(wc -l < "$out/changed-paths.txt" | tr -d ' ')
  if [[ -n $(git -C "$repo" ls-tree -r --name-only "$commit" -- .notes) ]]; then
    detail "files under .notes/ are tracked"
    return 1
  fi
  if git -C "$repo" log --format=%B "$base_commit..$commit" | grep -Ei "$ai_re"; then
    detail "a commit since $base has an AI attribution line"
    return 1
  fi
  line=$(git -C "$repo" show "$commit:CHANGELOG.md" | grep -m 1 '^## ')
  [[ $line =~ ^"## $version ("([0-9]{4}-[0-9]{2}-[0-9]{2})")"$ ]] ||
    { detail "CHANGELOG.md opens with '$line', not a dated $version heading"; return 1; }
  date=${BASH_REMATCH[1]}
  line=$(git -C "$repo" show "$commit:UPGRADING.md" | grep -m 1 '^## ')
  [[ $line == "## To $version" ]] || { detail "UPGRADING.md opens with '$line'"; return 1; }
  pins=$(git -C "$repo" show "$commit:README.md" | grep -o -- '--branch v[^ ]*' | sort -u)
  [[ $pins == "--branch $version" ]] || { detail "README.md pins: $(echo $pins)"; return 1; }
  git -C "$repo" show "$commit:README.md" | grep -qF "Installed crosshall $version " ||
    { detail "README.md does not show the installed $version"; return 1; }
  detail "$changed paths changed since $base; dated $date; no .notes/ or AI attribution"
}

row_schema() {
  local dir=internal/store/migrations edited opener=unchanged
  base_schema=$(git -C "$repo" ls-tree --name-only "$base_commit" "$dir/" | grep -c '\.sql$')
  head_schema=$(git -C "$repo" ls-tree --name-only "$commit" "$dir/" | grep -c '\.sql$')
  git -C "$repo" diff --name-status "$base_commit" "$commit" -- "$dir" internal/store/sqlite.go
  edited=$(git -C "$repo" diff --name-status "$base_commit" "$commit" -- "$dir" | grep -v '^A' | cut -f2-)
  [[ -z $edited ]] || { detail "an earlier migration changed: $(echo $edited)"; return 1; }
  ((head_schema >= base_schema)) || { detail "schema went down: $base_schema to $head_schema"; return 1; }
  git -C "$repo" diff --quiet "$base_commit" "$commit" -- internal/store/sqlite.go || opener=changed
  detail "schema $base_schema to $head_schema, only added migrations; store opener $opener"
}

row_install() {
  local clone=$work/install-src home=$work/install-home tagged
  needs_builds || return
  git clone -q --no-checkout -- "$repo" "$clone" && git -C "$clone" checkout -q --detach "$commit" ||
    { detail "could not clone the candidate"; return 1; }
  if tagged=$(git -C "$clone" rev-parse --verify --quiet "refs/tags/$version^{commit}"); then
    [[ $tagged == "$commit" ]] || { detail "tag $version points at another commit"; return 1; }
  else
    git -C "$clone" tag "$version"
  fi
  mkdir -m 700 -- "$home"
  # The module cache stands in for the network, as in scripts/ci-checks.sh.
  (cd -- "$clone" && env -i HOME="$home" PATH="$(dirname -- "$(command -v go)"):/usr/bin:/bin" \
    TMPDIR="$home" GOMODCACHE="$(go env GOMODCACHE)" CROSSHALL_TELEMETRY=off \
    bash scripts/install.sh --source "$home/bin" < /dev/null) || { detail "scripts/install.sh --source failed"; return 1; }
  [[ $("$home/bin/crosshall" version) == "$version" ]] ||
    { detail "installed binary reports $("$home/bin/crosshall" version)"; return 1; }
  detail "scripts/install.sh --source installed $version in a scratch home"
}

row_setup() {
  local root=$work/setup vendors=() vendor before after status
  needs_builds || return
  mkdir -p -m 700 -- "$root/home/.claude" "$root/home/.codex" "$root/vendor-bin" "$root/state"
  for vendor in claude codex; do
    command -v "$vendor" > /dev/null || continue
    ln -s -- "$(command -v "$vendor")" "$root/vendor-bin/$vendor"
    vendors+=("$vendor")
  done
  ((${#vendors[@]})) || { detail "neither claude nor codex is installed"; return 2; }
  printf '{\n  "theme": "dark"\n}\n' > "$root/home/.claude/settings.json"
  printf 'model = "gate"\n' > "$root/home/.codex/config.toml"
  before=$(cd -- "$root/home" && find .claude .codex -type f -exec shasum {} + | sort)
  started "$cand" "$root/state" "$root/home"
  hall "$root/state" "$root/home" PATH="$root/vendor-bin:/usr/bin:/bin" \
    "$cand" setup "${vendors[@]}" --apply < /dev/null || { detail "setup ${vendors[*]} --apply failed"; return 1; }
  status=$(hall "$root/state" "$root/home" PATH="$root/vendor-bin:/usr/bin:/bin" "$cand" --json setup status |
    json '" ".join(a["adapter"] for a in d["adapters"] if a["install"] == "installed" and a["owned"])')
  [[ $status == "${vendors[*]}" ]] || { detail "setup status shows installed: '$status'"; return 1; }
  hall "$root/state" "$root/home" "$cand" uninstall --apply < /dev/null || { detail "uninstall --apply failed"; return 1; }
  after=$(cd -- "$root/home" && find .claude .codex -type f -exec shasum {} + | sort)
  [[ $before == "$after" ]] || { detail "uninstall left the vendor configuration changed"; return 1; }
  hall "$root/state" "$root/home" "$cand" owner stop
  detail "setup ${vendors[*]} installed; uninstall restored the configuration byte for byte"
}

open_store() { # open_store <tree> <state-root>: the production opener, no dispatcher
  cp -- "$repo/scripts/release_gate_open.go" "$1/scripts/" || return
  (cd -- "$1" && go run ./scripts/release_gate_open.go "$2")
}
store_sound() { # store_sound <db> <schema>
  local got
  got=$(sqlite3 "$1" 'PRAGMA user_version') || return 1
  [[ $got == "$2" ]] || { echo "$1: schema $got, want $2"; return 1; }
  [[ $(sqlite3 "$1" 'PRAGMA integrity_check') == ok ]] || { echo "$1: integrity check failed"; return 1; }
  [[ -z $(sqlite3 "$1" 'PRAGMA foreign_key_check') ]] || { echo "$1: foreign key violations"; return 1; }
}
compared_tables=0
compared_rows=0
same_rows() { # same_rows <reference-db> <db>: every reference table, column, row and index survives
  local table cols line n m gone added name bad=0
  compared_tables=0
  compared_rows=0
  while IFS= read -r table; do
    cols=$(sqlite3 "$1" "SELECT group_concat(printf('\"%w\"', name), ',') FROM pragma_table_info('$table')")
    line=$(sqlite3 "$2" "ATTACH '$1' AS ref;
      SELECT (SELECT count(*) FROM ref.\"$table\"), (SELECT count(*) FROM main.\"$table\"),
        (SELECT count(*) FROM (SELECT $cols FROM ref.\"$table\" EXCEPT SELECT $cols FROM main.\"$table\")),
        (SELECT count(*) FROM (SELECT $cols FROM main.\"$table\" EXCEPT SELECT $cols FROM ref.\"$table\"));" 2>&1)
    IFS='|' read -r n m gone added <<< "$line"
    if [[ $n != "$m" || $gone != 0 || $added != 0 ]]; then
      echo "$table differs: $line"
      bad=1
    fi
    compared_tables=$((compared_tables + 1))
    compared_rows=$((compared_rows + ${n//[!0-9]/0}))
  done < <(sqlite3 "$1" "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
  while IFS= read -r name; do
    [[ $(sqlite3 "$2" "SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = '$name'") == 1 ]] ||
      { echo "index $name is missing"; bad=1; }
  done < <(sqlite3 "$1" "SELECT name FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL")
  return $bad
}

row_store() {
  local snap=$work/store baseline from backups backup rollback kept file files number status
  needs_builds || return
  [[ -n $head_schema ]] || { detail "needs the schema row"; return 2; }
  [[ -f $live_store ]] || { detail "no store at $live_store; pass --store"; return 2; }
  command -v sqlite3 > /dev/null || { detail "sqlite3 is not installed"; return 2; }
  mkdir -m 700 -- "$snap" "$snap/up" "$snap/rollback"
  baseline=$snap/baseline.sqlite3
  # An online backup through a read-only connection includes committed WAL data.
  sqlite3 -readonly "$live_store" ".backup '$baseline'" || { detail "could not snapshot the store"; return 1; }
  from=$(sqlite3 "$baseline" 'PRAGMA user_version')
  [[ $from == "$base_schema" ]] || { detail "the store is at schema $from, $base is at $base_schema"; return 2; }
  store_sound "$baseline" "$from" || { detail "the snapshot is not sound"; return 1; }
  shasum -a 256 "$baseline"

  cp -- "$baseline" "$snap/up/store.sqlite3" && chmod 600 "$snap/up/store.sqlite3"
  open_store "$head_tree" "$snap/up" || { detail "the candidate could not open the snapshot"; return 1; }
  store_sound "$snap/up/store.sqlite3" "$head_schema" || { detail "the upgraded store is not sound"; return 1; }
  # The migrations the store runs, which start before the base's for an older store.
  mkdir -m 700 -- "$snap/migrations"
  for file in $(git -C "$repo" ls-tree --name-only "$commit" internal/store/migrations/ | grep '\.sql$'); do
    number=${file##*/}
    ((10#${number%%_*} > from)) && git -C "$repo" show "$commit:$file" > "$snap/migrations/${file##*/}"
  done
  files=("$snap/migrations"/*.sql)
  [[ -e ${files[0]} ]] || files=()
  python3 -B "$repo/scripts/release_gate_rows.py" "$baseline" "$snap/up/store.sqlite3" ${files[@]+"${files[@]}"} > "$snap/rows.txt" 2>&1
  status=$?
  cat "$snap/rows.txt"
  ((status == 0)) || { detail "the upgrade changed rows no migration statement selects: $(head -n 1 "$snap/rows.txt")"; return 1; }
  kept=$(grep -h ' by its migrations$' "$snap/rows.txt" | sed 's/ by its migrations$//' | paste -sd ';' - | sed 's/;/; /g')
  kept="$(tail -n 1 "$snap/rows.txt" | sed 's/^compared //')${kept:+; changed by migrations: $kept}"
  backups=$(find "$snap/up" -name 'store-v*.sqlite3' | wc -l | tr -d ' ')
  if ((from < head_schema)); then
    [[ $backups == 1 ]] || { detail "$backups migration backups, want 1"; return 1; }
    backup=$(find "$snap/up" -name "store-v$from-*.sqlite3")
    [[ -n $backup ]] && same_rows "$baseline" "$backup" && same_rows "$backup" "$baseline" ||
      { detail "the migration backup differs from the snapshot"; return 1; }
  else
    [[ $backups == 0 ]] || { detail "$backups migration backups without a migration"; return 1; }
  fi
  open_store "$head_tree" "$snap/up" || { detail "the candidate could not reopen the store"; return 1; }
  [[ $(find "$snap/up" -name 'store-v*.sqlite3' | wc -l | tr -d ' ') == "$backups" ]] ||
    { detail "reopening made another backup"; return 1; }

  # Rollback: the base opens the migration backup, or the same store when the
  # schema did not change.
  rollback=$snap/rollback/store.sqlite3
  if ((from < head_schema)); then
    cp -- "$backup" "$rollback"
  else
    sqlite3 "$snap/up/store.sqlite3" ".backup '$rollback'"
  fi
  chmod 600 "$rollback"
  open_store "$base_tree" "$snap/rollback" || { detail "$base could not open the rollback store"; return 1; }
  store_sound "$rollback" "$((from < head_schema ? from : head_schema))" || { detail "the rollback store is not sound"; return 1; }
  same_rows "$baseline" "$rollback" || { detail "rollback lost rows"; return 1; }
  if ((from < head_schema)); then
    detail "schema $from to $head_schema: $kept; backup reopened under $base"
  else
    detail "schema $from unchanged: $kept; $base reopened the store"
  fi
}

row_continuity() {
  local root=$work/hall state home ask reply again messages backup
  needs_builds || return
  [[ -n $head_schema ]] || { detail "needs the schema row"; return 2; }
  state=$root/state
  home=$root/home
  mkdir -p -m 700 -- "$state" "$home"
  started "$cand" "$state" "$home"
  started "$base_bin" "$state" "$home"
  a() { hall "$state" "$home" CODEX_THREAD_ID=gate-a "$@"; }
  b() { hall "$state" "$home" CODEX_THREAD_ID=gate-b "$@"; }
  log_kinds() { hall "$state" "$home" "$1" --json owner log gate | json "[(m['kind'], m.get('in_reply_to')) for m in d['messages']]"; }

  hall "$state" "$home" "$base_bin" owner start && hall "$state" "$home" "$base_bin" owner channel create gate &&
    a "$base_bin" join gate --as a > /dev/null && b "$base_bin" join gate --as b > /dev/null ||
    { detail "$base could not start the hall"; return 1; }
  ask=$(a "$base_bin" --json ask @b "gate question" | json 'd["id"]') || { detail "$base could not ask"; return 1; }
  hall "$state" "$home" "$base_bin" owner stop

  hall "$state" "$home" "$cand" owner start || { detail "the candidate could not start on the $base hall"; return 1; }
  b "$cand" read "$ask" || { detail "the candidate could not read ask $ask"; return 1; }
  reply=$(b "$cand" --json --key gate-reply reply "$ask" "gate answer" | json 'd["id"]') &&
    again=$(b "$cand" --json --key gate-reply reply "$ask" "gate answer" | json 'd["id"]') ||
    { detail "the candidate could not reply"; return 1; }
  [[ $reply == "$again" ]] || { detail "a replayed reply key made reply $again after $reply"; return 1; }
  messages=$(log_kinds "$cand")
  [[ $messages == "[('ask', None), ('reply', $ask)]" ]] || { detail "hall holds $messages"; return 1; }
  hall "$state" "$home" "$cand" owner stop

  if ((base_schema < head_schema)); then
    backup=$(find "$state" -name "store-v$base_schema-*.sqlite3")
    [[ -n $backup ]] || { detail "no migration backup"; return 1; }
    rm -f -- "$state/store.sqlite3" "$state/store.sqlite3-wal" "$state/store.sqlite3-shm"
    cp -- "$backup" "$state/store.sqlite3" && chmod 600 "$state/store.sqlite3"
    hall "$state" "$home" "$base_bin" owner start || { detail "$base could not start on the backup"; return 1; }
    messages=$(log_kinds "$base_bin")
    [[ $messages == "[('ask', None)]" ]] || { detail "the restored hall holds $messages"; return 1; }
    [[ $(b "$base_bin" --json inbox | json '[m["id"] for m in d["unread"]]') == "[$ask]" ]] ||
      { detail "ask $ask is not unread after the rollback"; return 1; }
    hall "$state" "$home" "$base_bin" owner stop
    detail "ask $ask settled once by reply $reply under $version, key replay kept it; $base restored the open ask"
  else
    hall "$state" "$home" "$base_bin" owner start || { detail "$base could not start on the candidate's store"; return 1; }
    again=$(b "$base_bin" --json --key gate-reply reply "$ask" "gate answer" | json 'd["id"]')
    [[ $again == "$reply" ]] || { detail "$base replayed the reply key as $again"; return 1; }
    messages=$(log_kinds "$base_bin")
    [[ $messages == "[('ask', None), ('reply', $ask)]" ]] || { detail "$base sees $messages"; return 1; }
    hall "$state" "$home" "$base_bin" owner stop
    detail "ask $ask settled once by reply $reply; key replay held under $version and $base"
  fi
}

# The candidate's median cost must stay within this factor of the base's.
hook_cost_limit=1.5
row_hook_cost() {
  local test=TestHookCostStaysBoundedInALongSession side round base_ns head_ns
  needs_builds || return
  for side in base head; do
    (cd -- "$work/tree/$side" && go test -c -o "$work/cost-$side.test" ./internal/domain) ||
      { detail "could not build the $side tests"; return 1; }
  done
  "$work/cost-base.test" -test.list "^$test\$" | grep -qx "$test" || { detail "$base has no $test"; return 2; }
  # Interleaved runs, so that a change of load reaches both sides alike.
  for round in 1 2 3; do
    for side in base head; do
      (cd -- "$work/tree/$side/internal/domain" && "$work/cost-$side.test" -test.run "^$test\$" -test.v -test.count=1) |
        tee "$work/cost-$side-$round.log"
      if ((${PIPESTATUS[0]})); then
        detail "$test failed on $side in run $round"
        return 1
      fi
    done
  done
  median500() { # nanoseconds of the median 500-turn sample of three runs
    cat "$work"/cost-"$1"-*.log | awk '$2 == "500" && $3 == "turns:" {
        v = $4; f = 1
        if (v ~ /ms$/) { f = 1e6 } else if (v ~ /µs$/) { f = 1e3 } else if (v ~ /ns$/) { f = 1 } else if (v ~ /s$/) { f = 1e9 }
        gsub(/[^0-9.]/, "", v); print v * f }' | sort -n | sed -n 2p
  }
  base_ns=$(median500 base)
  head_ns=$(median500 head)
  [[ -n $base_ns && -n $head_ns ]] || { detail "could not read the 500-turn medians"; return 1; }
  awk -v b="$base_ns" -v h="$head_ns" -v l="$hook_cost_limit" 'BEGIN {
      printf "500 turns: %s %.3f ms, candidate %.3f ms, ratio %.2f (limit %s), median of 3 interleaved runs\n", "base", b / 1e6, h / 1e6, h / b, l
      exit !(h / b <= l) }' > "$out/$current.detail"
}

row_cross_vendor() {
  local test=TestCrossVendorRoundTrip events=$out/cross-vendor.json status verdict judged
  needs_builds || return
  grep -q "^func $test(" "$head_tree"/test/conformance/*_test.go || { detail "the candidate has no $test"; return 2; }
  command -v claude > /dev/null && command -v codex > /dev/null || { detail "$test needs claude and codex on PATH"; return 2; }
  (cd -- "$head_tree" && CROSSHALL_CONFORMANCE_CROSSHALL="$cand" go test -tags conformance -count=1 -timeout 3m \
    -run "^$test\$" -json ./test/conformance/) > "$events"
  status=$?
  # Each direction must pass on its own; a skipped one is not a pass, and a
  # failed go test is a failure whatever its events say.
  verdict=$(python3 -B "$repo/scripts/release_gate_events.py" "$test" claude_to_codex codex_to_claude < "$events")
  judged=$?
  printf 'go test exit %s; %s\n' "$status" "$verdict"
  detail "$verdict"
  ((status == 0)) || { detail "go test exit $status; $verdict"; return 1; }
  return $judged
}

row_full_run() {
  local lock start status
  $full_run || { detail "skipped by --skip-full-run"; return 3; }
  needs_builds || return
  lock=$(git -C "$repo" rev-parse --path-format=absolute --git-common-dir)/ci-checks.lock
  start=$(date +%s)
  # One full run at a time on this machine, behind the lock other runs use.
  if [[ -x /usr/bin/lockf ]]; then
    (cd -- "$head_tree" && /usr/bin/lockf -k "$lock" bash scripts/ci-checks.sh)
  else
    (cd -- "$head_tree" && flock "$lock" bash scripts/ci-checks.sh)
  fi
  status=$?
  detail "exit $status after $((($(date +%s) - start) / 60))m $((($(date +%s) - start) % 60))s; last line: $(tail -n 1 "$out/$current.log")"
  ((status == 0)) || return 1
}

row 'Pin and reproducible build' row_build
row 'Source, attribution and release dating' row_source
row 'Schema and migrations' row_schema
row 'Install from source' row_install
row 'Setup and uninstall' row_setup
row 'Store snapshot upgrade and rollback' row_store
row 'CLI continuity across upgrade' row_continuity
row 'Hook cost against the base' row_hook_cost
row 'Cross-vendor round trip' row_cross_vendor
row 'One locked full run' row_full_run

verdict=PASS
for result in "${results[@]}"; do
  case $result in
    FAIL) verdict=FAIL ;;
    BLOCKED | SKIP) [[ $verdict == FAIL ]] || verdict=INCOMPLETE ;;
  esac
done
{
  printf '# Release gate: %s\n\n' "$version"
  printf 'Candidate `%s`, base %s `%s`, %s.\n' "$commit" "$base" "$base_commit" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'Evidence: `%s`. New-feature rows stay manual.\n\n' "$out"
  printf '| Row | Result | Detail |\n|---|---|---|\n'
  for i in "${!titles[@]}"; do
    printf '| %s | %s | %s |\n' "${titles[$i]}" "${results[$i]}" "${details[$i]//|/\\|}"
  done
  printf '\n**%s**\n' "$verdict"
} | tee "$out/summary.md"
case $verdict in
  PASS) exit 0 ;;
  FAIL) exit 1 ;;
  *) exit 3 ;;
esac
