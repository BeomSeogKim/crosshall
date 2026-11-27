#!/usr/bin/env bash
# Exercise the installer against local tags without downloading any modules.
set -euo pipefail

scripts=$(cd -- "$(dirname -- "$0")" && pwd -P)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/crosshall-install-test.XXXXXXXX")
trap 'rm -rf -- "$tmp"' EXIT
tmp=$(cd -- "$tmp" && pwd -P)
repo="$tmp/repo"
dest="$tmp/installer's bin"
target="$dest/crosshall"
mkdir -p "$repo/cmd/crosshall" "$repo/scripts" "$tmp/home"
cp "$scripts/install.sh" "$repo/scripts/"
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
export GOPROXY=off GOSUMDB=off GOENV=off GOTOOLCHAIN=local
unset GOFLAGS

fail() { echo "install_test: $*" >&2; exit 1; }

install_release() {
  env HOME="$tmp/home" XDG_STATE_HOME="$tmp/xdg" CROSSHALL_STATE_DIR="$tmp/state" \
    CLAUDE_CONFIG_DIR="$tmp/home/.claude" CODEX_HOME="$tmp/home/.codex" \
    INSTALL_TEST_MARKER="$tmp/executed" "$repo/scripts/install.sh" --source "$@"
}

expect_refusal() {
  if install_release "$@" > "$tmp/output" 2>&1; then
    fail "expected refusal for $*"
  fi
}

check_output_line() {
  grep -Fx -- "$1" "$tmp/output" >/dev/null || fail "missing output line '$1': $(cat "$tmp/output")"
}

# Replace the installed file by renaming a sibling over it. Writing into it
# would reopen a program that has just run, which Linux can refuse as busy.
replace_target() {
  cp -- "$1" "$dest/.replacement"
  mv -f -- "$dest/.replacement" "$target"
}

check_release() {
  [[ -f $target && ! -L $target ]] || fail "expected a regular installed executable"
  [[ $("$target" version) == "$1" ]] || fail "expected installed version $1"
  [[ $("$target" identify) == "$2" ]] || fail "expected program from $2 release"
}

git -C "$repo" init -q
git -C "$repo" config user.name Fixture
git -C "$repo" config user.email fixture@example.invalid
cat > "$repo/go.mod" <<'EOF'
module github.com/BeomSeogKim/crosshall

go 1.24
EOF
cat > "$repo/cmd/crosshall/main.go" <<'EOF'
package main

import "os"

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		// Fail the first version call as a busy file does (exit 126) and
		// record each call, so the test sees the installer retry once.
		if calls := os.Getenv("INSTALL_TEST_BUSY_ONCE"); calls != "" {
			seen, _ := os.ReadFile(calls)
			if err := os.WriteFile(calls, append(seen, '.'), 0o600); err != nil {
				os.Exit(1)
			}
			if len(seen) == 0 {
				os.Exit(126)
			}
		}
		if release == "first" {
			switch os.Getenv("INSTALL_TEST_BAD_VERSION") {
			case "malformed":
				os.Stdout.WriteString("unknown release\n")
				return
			case "error":
				os.Stdout.WriteString(version + "\n")
				os.Exit(1)
			}
		}
		os.Stdout.WriteString(version + "\n")
		return
	}
	os.Stdout.WriteString(release + "\n")
}
EOF
tag_release() {
  printf 'package main\nconst release = "%s"\n' "$2" > "$repo/cmd/crosshall/release.go"
  git -C "$repo" add .
  git -C "$repo" commit -qm "$2 release"
  git -C "$repo" tag "$1"
}

git -C "$repo" commit --allow-empty -qm 'no release tags yet'
expect_refusal "$dest"
check_output_line '  git fetch --tags'

tag_release v0.1.0 first
# A just-built binary can briefly refuse to run on Linux ("text file busy",
# exit 126). The installer retries; this release fails its first call so.
INSTALL_TEST_BUSY_ONCE="$tmp/version-calls" install_release "$dest" > "$tmp/output" 2>&1 ||
  fail "installer gave up after exit 126: $(cat "$tmp/output")"
[[ $(cat "$tmp/version-calls") == .. ]] || fail "expected the version check to retry once after exit 126"
check_release v0.1.0 first
check_output_line "Installed crosshall v0.1.0 at $target"
printf -v absolute_start '  %q owner start' "$target"
check_output_line "$absolute_start"
printf -v path_advice '%s is not on PATH: add export PATH=%q:"$PATH" to your shell startup file.' "$dest" "$dest"
check_output_line "$path_advice"
cp "$target" "$tmp/first"

git -C "$repo" tag v0.2.0-rc.9
git -C "$repo" tag -am 'synthetic release candidate' v0.2.0-rc.11
git -C "$repo" commit --allow-empty -qm 'after release candidates'
expect_refusal "$dest"
check_output_line '  git switch --detach refs/tags/v0.2.0-rc.11'

tag_release v0.2.0 second
git -C "$repo" tag -am 'synthetic release candidate at the same commit' v0.2.0-rc.12
tree=$(git -C "$repo" rev-parse 'HEAD^{tree}')
unrelated=$(git -C "$repo" commit-tree "$tree" -m 'unrelated release')
git -C "$repo" tag v9.0.0 "$unrelated"
git -C "$repo" commit --allow-empty -qm 'after the final release'
git -C "$repo" tag v99.0.0.not-a-release
expect_refusal "$dest"
check_output_line '  git switch --detach refs/tags/v0.2.0'
cmp "$tmp/first" "$target" || fail 'off-tag refusal changed the installed binary'

untagged=$(git -C "$repo" commit-tree "$tree" -m 'no release in this history')
git -C "$repo" switch --quiet --detach "$untagged"
expect_refusal "$dest"
check_output_line '  git switch --detach refs/tags/v9.0.0'
git -C "$repo" switch --quiet --detach refs/tags/v0.2.0

printf 'package main\nconst release = "working tree"\n' > "$repo/cmd/crosshall/release.go"
printf 'invalid Go source\n' > "$repo/cmd/crosshall/untracked.go"
for bad_version in malformed error; do
  INSTALL_TEST_BAD_VERSION=$bad_version expect_refusal "$dest"
  cmp "$tmp/first" "$target" || fail "binary changed after $bad_version version response"
done
PATH="$dest:$PATH" GOFLAGS=-overlay=missing-env-overlay.json install_release "$dest" > "$tmp/output"
check_release v0.2.0 second
check_output_line "Installed crosshall v0.2.0 at $target"
check_output_line '  crosshall owner start'

cat > "$tmp/unrelated" <<'EOF'
#!/bin/sh
echo ran > "$INSTALL_TEST_MARKER"
echo v0.1.0
EOF
chmod +x "$tmp/unrelated"
replace_target "$tmp/unrelated"
expect_refusal "$dest"
[[ ! -e $tmp/executed ]] || fail "existing program was executed during inspection"
cmp "$target" "$tmp/unrelated" || fail "existing program changed on refusal"
printf 'GOFLAGS=-overlay=missing-config-overlay.json\n' > "$tmp/go.env"
GOENV="$tmp/go.env" install_release --force "$dest" > "$tmp/output"
check_release v0.2.0 second

replace_target "$tmp/first"
chmod 600 "$target"
expect_refusal "$dest"
[[ ! -x $target ]] && cmp "$tmp/first" "$target" || fail "non-executable file changed"
install_release --force "$dest" > "$tmp/output"
check_release v0.2.0 second
rm "$target"

mkfifo "$target"
# This writer blocks until a reader opens the FIFO. On a regression, it
# records the read and closes the pipe so the installer can finish.
(exec 3>"$target"; echo opened > "$tmp/fifo-read") &
writer=$!
status=0
install_release "$dest" > "$tmp/output" 2>&1 || status=$?
kill "$writer" 2>/dev/null || true
wait "$writer" 2>/dev/null || true
[[ $status != 0 && -p $target && ! -e $tmp/fifo-read ]] || fail "installer accepted or read FIFO"
rm "$target"

mkdir "$tmp/directory"
echo untouched > "$tmp/directory/marker"
for link_target in "$tmp/first" "$tmp/absent" "$tmp/directory"; do
  ln -s "$link_target" "$target"
  expect_refusal "$dest"
  [[ -L $target && $(readlink "$target") == "$link_target" ]] || fail "link changed on refusal"
  install_release --force "$dest" > "$tmp/output"
  check_release v0.2.0 second
  rm "$target"
done
[[ $("$tmp/first" identify) == first ]] || fail "file link target changed"
[[ ! -e $tmp/absent ]] || fail "dangling link target was created"
[[ $(cat "$tmp/directory/marker") == untouched && ! -e $tmp/directory/crosshall ]] ||
  fail "directory link target changed"

mkdir "$target"
echo untouched > "$target/marker"
expect_refusal "$dest"
expect_refusal --force "$dest"
[[ -d $target && $(cat "$target/marker") == untouched && ! -e $target/crosshall ]] ||
  fail "existing directory changed"
echo 'installer checks passed'
