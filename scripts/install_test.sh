#!/usr/bin/env bash
# Exercise the installer against two local tags without downloading any modules.
set -euo pipefail

scripts=$(cd -- "$(dirname -- "$0")" && pwd -P)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/crosshall-install-test.XXXXXXXX")
trap 'rm -rf -- "$tmp"' EXIT
repo="$tmp/repo"
dest="$tmp/bin"
target="$dest/crosshall"
mkdir -p "$repo/cmd/crosshall" "$repo/scripts" "$tmp/home"
cp "$scripts/install.sh" "$repo/scripts/"
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
export GOPROXY=off GOSUMDB=off GOENV=off GOTOOLCHAIN=local
unset GOFLAGS

fail() { echo "install_test: $*" >&2; exit 1; }

install_release() {
  env HOME="$tmp/home" INSTALL_TEST_MARKER="$tmp/executed" "$repo/scripts/install.sh" "$@"
}

expect_refusal() {
  if install_release "$@" > "$tmp/output" 2>&1; then
    fail "expected refusal for $*"
  fi
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

tag_release v0.1.0 first
install_release "$dest" > "$tmp/output"
check_release v0.1.0 first
cp "$target" "$tmp/first"

tag_release v0.2.0 second
printf 'package main\nconst release = "working tree"\n' > "$repo/cmd/crosshall/release.go"
printf 'invalid Go source\n' > "$repo/cmd/crosshall/untracked.go"
for bad_version in malformed error; do
  INSTALL_TEST_BAD_VERSION=$bad_version expect_refusal "$dest"
  cmp "$tmp/first" "$target" || fail "binary changed after $bad_version version response"
done
GOFLAGS=-overlay=missing-env-overlay.json install_release "$dest" > "$tmp/output"
check_release v0.2.0 second

cat > "$target" <<'EOF'
#!/bin/sh
echo ran > "$INSTALL_TEST_MARKER"
echo v0.1.0
EOF
chmod +x "$target"
cp "$target" "$tmp/unrelated"
expect_refusal "$dest"
[[ ! -e $tmp/executed ]] || fail "existing program was executed during inspection"
cmp "$target" "$tmp/unrelated" || fail "existing program changed on refusal"
printf 'GOFLAGS=-overlay=missing-config-overlay.json\n' > "$tmp/go.env"
GOENV="$tmp/go.env" install_release --force "$dest" > "$tmp/output"
check_release v0.2.0 second

cp "$tmp/first" "$target"
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
