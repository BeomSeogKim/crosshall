#!/usr/bin/env bash
# Run the download and source-fallback paths with local assets and no network.
set -euo pipefail
scripts=$(cd -- "$(dirname -- "$0")" && pwd -P)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/crosshall-binary-test.XXXXXXXX")
trap 'rm -rf -- "$tmp"' EXIT
repo="$tmp/repo"
tools="$tmp/tools"
assets="$tmp/assets"
payload="$tmp/payload"
mkdir -p "$repo/scripts" "$tools" "$assets" "$payload" "$tmp/test home"
cp "$scripts/install.sh" "$repo/scripts/"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

fail() { echo "install_binary_test: $*" >&2; exit 1; }
for tool in awk bash cat chmod cmp dirname git grep ln mkdir mktemp mv rm sleep tar uname; do
  ln -s "$(command -v "$tool")" "$tools/$tool"
done
if tar --version | grep -q 'GNU tar'; then
  ln -s "$(command -v gzip)" "$tools/gzip"
fi
if command -v sha256sum >/dev/null; then
  ln -s "$(command -v sha256sum)" "$tools/sha256sum"
  digest() { sha256sum < "$1"; }
else
  ln -s "$(command -v shasum)" "$tools/shasum"
  digest() { shasum -a 256 < "$1"; }
fi
case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail 'run on a supported operating system' ;;
esac
case $(uname -m) in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) fail 'run on a supported architecture' ;;
esac
asset="crosshall-$os-$arch.tar.gz"
version=v0.8.0
printf 'module github.com/BeomSeogKim/crosshall\n\ngo 1.27.4\n' > "$repo/go.mod"
cat > "$repo/from-source" <<'SOURCE'
#!/bin/sh
printf 'source\n' >> "$TEST_EXECUTIONS"
printf 'v0.8.0\n'
SOURCE
chmod +x "$repo/from-source"
cat > "$payload/crosshall" <<'BINARY'
#!/bin/sh
printf 'archive\n' >> "$TEST_EXECUTIONS"
printf 'v0.8.0\n'
BINARY
chmod +x "$payload/crosshall"
git -C "$repo" init -q
git -C "$repo" add .
git -C "$repo" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm fixture
git -C "$repo" tag "$version"
git -C "$repo" rev-parse HEAD > "$payload/COMMIT"

publish_fixture() {
  tar -czf "$assets/$asset" -C "$payload" COMMIT crosshall
  hash=$(digest "$assets/$asset")
  printf '%s  %s\n' "${hash%% *}" "$asset" > "$assets/$asset.sha256"
}
publish_fixture
cat > "$tools/curl" <<'FETCH'
#!/bin/sh
set -eu
report_status=false
while [ "$#" -gt 2 ]; do
  case "$1" in
    --write-out) [ "$2" = '%{http_code}' ]; report_status=true ;;
  esac
  shift
done
destination=$1
url=$2
respond() {
  if [ "$report_status" = true ]; then printf '%s' "$1"; fi
  exit "$2"
}
case "$url" in
  "https://github.com/BeomSeogKim/crosshall/releases/download/v0.8.0/$TEST_ASSET"|\
  "https://github.com/BeomSeogKim/crosshall/releases/download/v0.8.0/$TEST_ASSET.sha256") ;;
  *) printf 'unexpected URL: %s\n' "$url" >> "$TEST_FETCHES"; exit 1 ;;
esac
file=${url##*/}
printf '%s\n' "$file" >> "$TEST_FETCHES"
case "$TEST_CASE:$file" in
  missing:*.sha256|missing-archive:"$TEST_ASSET") respond 404 22 ;;
  missing-http2:*.sha256) respond 404 56 ;;
  checksum-blocked:*.sha256) respond 403 22 ;;
  checksum-network:*.sha256) respond 000 7 ;;
  checksum-timeout:*.sha256) respond 000 28 ;;
  bad:*.sha256)
    printf '%064d  %s\n' 0 "$TEST_ASSET" > "$destination"
    respond 200 0 ;;
esac
cat "$TEST_ASSETS/$file" > "$destination"
respond 200 0
FETCH
chmod +x "$tools/curl"

install() {
  : > "$tmp/executions"
  : > "$tmp/fetches"
  test_case=$1
  shift
  env HOME="$tmp/test home" PATH="$tools" CI=1 TEST_CASE="$test_case" TEST_ASSET="$asset" \
    TEST_ASSETS="$assets" TEST_EXECUTIONS="$tmp/executions" TEST_FETCHES="$tmp/fetches" \
    bash "$repo/scripts/install.sh" "$@" > "$tmp/output" 2> "$tmp/errors"
}

[[ ! -e $tools/go ]] || fail 'compilerless fixture unexpectedly has Go'
install good "$tmp/install location" || fail "verified install failed: $(cat "$tmp/errors")"
target="$tmp/install location/crosshall"
cmp -s "$target" "$payload/crosshall" || fail 'verified archive was not installed'
[[ $(cat "$tmp/executions") == archive ]] || fail 'wrong verified program'
asset_requests=$(printf '%s\n' "$asset.sha256" "$asset")
[[ $(cat "$tmp/fetches") == "$asset_requests" ]] || fail 'verified install made unexpected network calls'
# The installation receipt gains a path and next steps with the installer
# guidance change. Require the exact release version in either receipt.
case $(cat "$tmp/output") in
  "$version"|"Installed crosshall $version at "*) ;;
  *) fail "verified install did not report $version" ;;
esac
install good "$tmp/install location" || fail 'same verified artifact could not be reinstalled without Go'
install missing "$tmp/unverified" && fail 'installed despite missing checksum and compiler'
[[ ! -e $tmp/unverified/crosshall && ! -s $tmp/executions ]] || fail 'unverified program ran or installed'
grep -q 'source build needs Go' "$tmp/errors" || fail 'missing source toolchain guidance'

# A different incumbent is never run to identify it on the no-Go path.
cp "$repo/from-source" "$target"
if install good "$tmp/install location"; then
  fail 'compilerless install accepted a different incumbent without --force'
fi
[[ $(cat "$tmp/executions") == archive ]] || fail 'unrecognized incumbent was run'
cmp -s "$target" "$repo/from-source" || fail 'unrecognized incumbent changed'
install good --force "$tmp/install location" || fail 'compilerless explicit replacement failed'

# A fake Go build marks which executable used the fallback, without
# downloading modules or changing the real build cache.
cat > "$tools/go" <<'GO'
#!/bin/sh
set -eu
case "$1" in
  env) printf '%s\n' /nonexistent ;;
  build)
    until [ "$1" = -o ]; do shift; done
    cat from-source > "$2"
    chmod 755 "$2" ;;
  *) exit 1 ;;
esac
GO
chmod +x "$tools/go"
checksum_reason="no verified release archive found for $version on $os/$arch because the checksum is unavailable"
for test_case in bad missing missing-http2 checksum-blocked checksum-network checksum-timeout missing-archive wrong-commit; do
  expected_requests=$asset_requests
  case "$test_case" in
    bad) reason='checksum mismatch' ;;
    missing|missing-http2) reason="no release archive is published for $version on $os/$arch"; expected_requests=$asset.sha256 ;;
    checksum-blocked) reason=$checksum_reason; expected_requests=$asset.sha256 ;;
    checksum-network|checksum-timeout) reason="could not reach the release for $version on $os/$arch"; expected_requests=$asset.sha256 ;;
    missing-archive) reason="release archive unavailable for $version on $os/$arch" ;;
    wrong-commit)
      reason='binary archive does not match this checkout'
      echo different-checkout > "$payload/COMMIT"
      publish_fixture ;;
  esac
  install "$test_case" "$tmp/$test_case" || fail "no fallback for $test_case: $(cat "$tmp/errors")"
  cmp -s "$tmp/$test_case/crosshall" "$repo/from-source" || fail "unverified binary installed for $test_case"
  [[ $(cat "$tmp/executions") == source ]] || fail "unverified binary ran for $test_case"
  grep -Fq "$reason); falling back to a source build" "$tmp/errors" || fail "no fallback reason for $test_case"
  [[ $(cat "$tmp/fetches") == "$expected_requests" ]] || fail "unexpected network calls for $test_case"
done

if [[ -L $tools/gzip ]]; then
  rm -- "$tools/gzip"
  install good "$tmp/no-gzip" || fail "no fallback without gzip: $(cat "$tmp/errors")"
  cmp -s "$tmp/no-gzip/crosshall" "$repo/from-source" || fail 'did not install from source without gzip'
  [[ $(cat "$tmp/executions") == source ]] || fail 'archive executable ran without gzip'
  grep -Fq 'cannot read the verified gzip archive with tar); falling back to a source build' "$tmp/errors" ||
    fail "wrong diagnostic without gzip: $(cat "$tmp/errors")"
fi
printf '%s\n' 'binary installer checks passed'
