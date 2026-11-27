#!/usr/bin/env bash
# Run the README's install example as written, against a local repository at
# the README's tag and local release assets, and compare what the installer
# prints with the receipt the README shows. No network: git clone is
# redirected to the local repository, and curl serves the local assets.
set -euo pipefail
scripts=$(cd -- "$(dirname -- "$0")" && pwd -P)
root=$(dirname -- "$scripts")
readme="$root/README.md"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/crosshall-readme-install.XXXXXXXX")
trap 'rm -rf -- "$tmp"' EXIT
tmp=$(cd -- "$tmp" && pwd -P)
fail() { echo "readme_install_test: $*" >&2; exit 1; }

# The block after a marker line, without its fences.
block() {
  awk -v marker="$1" '
    $0 == marker { found = 1; next }
    found && /^```/ { if (open) exit; open = 1; next }
    open { print }
  ' "$readme"
}
block '<!-- install-example -->' > "$tmp/example.sh"
block '<!-- install-receipt -->' > "$tmp/receipt"
[[ -s $tmp/example.sh && -s $tmp/receipt ]] || fail 'README has no marked install example and receipt'
clone=$(grep '^git clone ' "$tmp/example.sh") || fail 'install example has no git clone'
# The one-line install near the top of the page is the same example.
oneline=$(awk 'NF { printf "%s%s", sep, $0; sep = " && " }' "$tmp/example.sh")
grep -Fq -- "\`$oneline\`" "$readme" || fail "the README's one-line install differs from the install example: $oneline"
[[ $clone =~ --branch\ ([^ ]+)\ .*\ (https://[^ ]+)$ ]] || fail "unexpected clone line: $clone"
tag=${BASH_REMATCH[1]}
remote=${BASH_REMATCH[2]}
# Only the project's own repository is redirected to the local fixture.
[[ $remote == https://github.com/BeomSeogKim/crosshall.git ]] || fail "the install example clones $remote, not the project repository"

# The repository the README clones, at the README's tag, with the installer
# that tag shipped: the README describes the release a reader installs, not
# this tree. Before the tag exists (a release being prepared), this tree is
# that release, so its installer stands in.
upstream="$tmp/upstream"
mkdir -p "$upstream/scripts"
if git -C "$root" rev-parse -q --verify "refs/tags/$tag^{commit}" >/dev/null; then
  installer="the $tag tag"
  git -C "$root" show "$tag:scripts/install.sh" > "$upstream/scripts/install.sh"
  chmod +x "$upstream/scripts/install.sh"
else
  installer="this tree (no $tag tag in this clone)"
  cp "$scripts/install.sh" "$upstream/scripts/"
fi
printf 'module github.com/BeomSeogKim/crosshall\n\ngo 1.27.4\n' > "$upstream/go.mod"
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="$tmp/gitconfig"
git config --file "$GIT_CONFIG_GLOBAL" "url.file://$upstream.insteadOf" "$remote"
git -C "$upstream" init -q
git -C "$upstream" add .
git -C "$upstream" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm fixture
git -C "$upstream" tag "$tag"

case $(uname -s) in Darwin) os=darwin ;; Linux) os=linux ;; *) fail 'unsupported system' ;; esac
case $(uname -m) in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) fail 'unsupported machine' ;; esac
asset="crosshall-$os-$arch.tar.gz"
assets="$tmp/assets"
payload="$tmp/payload"
mkdir -p "$assets" "$payload"
printf '#!/bin/sh\nprintf "%%s\\n" %q\n' "$tag" > "$payload/crosshall"
chmod +x "$payload/crosshall"
git -C "$upstream" rev-parse HEAD > "$payload/COMMIT"
tar -czf "$assets/$asset" -C "$payload" COMMIT crosshall
if command -v sha256sum >/dev/null; then hash=$(sha256sum < "$assets/$asset"); else hash=$(shasum -a 256 < "$assets/$asset"); fi
printf '%s  %s\n' "${hash%% *}" "$asset" > "$assets/$asset.sha256"

# curl serves only this release's two assets.
tools="$tmp/tools"
mkdir -p "$tools"
cat > "$tools/curl" <<FETCH
#!/bin/sh
set -eu
while [ "\$#" -gt 2 ]; do shift; done
case "\$2" in
  https://github.com/BeomSeogKim/crosshall/releases/download/$tag/$asset|\\
  https://github.com/BeomSeogKim/crosshall/releases/download/$tag/$asset.sha256) ;;
  *) echo "unexpected URL: \$2" >&2; exit 1 ;;
esac
cat "$assets/\${2##*/}" > "\$1"
FETCH
chmod +x "$tools/curl"

home="$tmp/home"
mkdir -p "$home"
(cd "$home" && env HOME="$home" PATH="$tools:/usr/bin:/bin" CI=1 bash -e "$tmp/example.sh") > "$tmp/output" 2> "$tmp/errors" ||
  fail "install example failed: $(cat "$tmp/errors")"
# The README writes the home directory as /home/you.
sed "s#$home#/home/you#g" "$tmp/output" > "$tmp/shown"
cmp -s "$tmp/shown" "$tmp/receipt" ||
  fail "the installer of $installer printed:
$(cat "$tmp/shown")
README shows:
$(cat "$tmp/receipt")"
cmp -s "$home/.local/bin/crosshall" "$payload/crosshall" || fail 'the release binary was not installed'
printf '%s\n' 'README install example checks passed'
