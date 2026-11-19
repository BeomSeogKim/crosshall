#!/usr/bin/env bash
# Install the release tag at HEAD. Local source edits do not enter the binary.
set -euo pipefail

usage='Usage: scripts/install.sh [--force] [--source] [install-directory]  (default: ~/.local/bin)'
force=false
from_source=false
dest=
for arg in "$@"; do
  case $arg in
    --help|-h)
      echo "$usage"
      echo 'Use --force to replace an unrecognized file or a symbolic link itself.'
      echo 'Use --source to skip the release download and build with Go.'
      exit 0 ;;
    --force) force=true ;;
    --source) from_source=true ;;
    -*) echo "$usage" >&2; exit 2 ;;
    *) [[ -z $dest && -n $arg ]] || { echo "$usage" >&2; exit 2; }
       dest=$arg ;;
  esac
done

repo=$(cd -- "$(dirname -- "$0")/.." && pwd -P)
commit=$(git -C "$repo" rev-parse --verify HEAD)
release='^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$'
release_tag() {
  local candidate
  # A final release sorts ahead of the same version's prereleases.
  while IFS= read -r candidate; do
    if [[ $candidate =~ $release ]] &&
        git -C "$repo" rev-parse --verify --quiet "refs/tags/$candidate^{commit}" >/dev/null; then
      printf '%s\n' "$candidate"
      return
    fi
  done < <(git -C "$repo" -c versionsort.suffix=- -c versionsort.suffix=+ \
    tag --list --sort=-version:refname "$@" 'v*')
  return 1
}
if ! tag=$(release_tag --points-at "$commit"); then
  echo 'This checkout is not at a release tag.' >&2
  if tag=$(release_tag --merged "$commit") || tag=$(release_tag); then
    printf 'From the checkout root, run these commands to install %s:\n' "$tag" >&2
    printf '  git switch --detach refs/tags/%s\n  ./scripts/install.sh\n' "$tag" >&2
  else
    printf 'No local release tags. Fetch tags from the checkout root and rerun the installer:\n  git fetch --tags\n' >&2
  fi
  exit 1
fi

dest=${dest:-"$HOME/.local/bin"}
mkdir -p -- "$dest"
dest=$(cd -- "$dest" && pwd -P)
target=$dest/crosshall
if [[ ! -L $target && -d $target ]]; then
  echo "Cannot replace directory $target with an executable." >&2
  exit 1
fi
stage=$(mktemp -d "$dest/.crosshall-install.XXXXXXXX")
trap 'rm -rf -- "$stage"' EXIT
mkdir "$stage/telemetry"
printf 'off\n' > "$stage/telemetry/mode"
build_go() {
  GOENV=off GOFLAGS= GOTOOLCHAIN=local GOWORK=off TEST_TELEMETRY_DIR="$stage/telemetry" go "$@"
}

check_incumbent() {
  local installed
  [[ ! -L $stage/incumbent && -f $stage/incumbent && -x $stage/incumbent ]] || return 1
  # Without Go, compare the stable inode with the verified candidate below.
  command -v go >/dev/null || return 0
  build_go version -m "$stage/incumbent" | grep -Fx $'\tpath\tgithub.com/BeomSeogKim/crosshall/cmd/crosshall' >/dev/null &&
    installed=$("$stage/incumbent" version) && [[ $installed == dev || $installed =~ $release ]]
}

if [[ $force == false && ( -L $target || -e $target ) ]]; then
  # Inspect and query the same inode, rather than resolving the path twice.
  ln -P -- "$target" "$stage/incumbent"
  if ! check_incumbent; then
    echo "Unrecognized entry at $target. Use --force to replace it." >&2
    exit 1
  fi
fi

fetch_prebuilt() {
  local os arch asset url checksum expected actual archived_commit
  case $(uname -s) in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) prebuilt_reason='unsupported operating system'; return 1 ;;
  esac
  case $(uname -m) in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) prebuilt_reason='unsupported architecture'; return 1 ;;
  esac
  command -v curl >/dev/null || { prebuilt_reason='curl is unavailable'; return 1; }
  asset="crosshall-$os-$arch.tar.gz"
  url="https://github.com/BeomSeogKim/crosshall/releases/download/$tag/$asset"
  if ! fetch "$url.sha256" "$stage/checksum"; then
    prebuilt_reason="no verified release archive found for $tag on $os/$arch because the checksum is unavailable"
    return 1
  fi
  checksum=$(cat "$stage/checksum") || { prebuilt_reason='checksum unreadable'; return 1; }
  expected=${checksum%% *}
  if ! [[ $expected =~ ^[0-9a-f]{64}$ && $checksum == "$expected  $asset" ]]; then
    prebuilt_reason='invalid checksum file'
    return 1
  fi
  if ! fetch "$url" "$stage/$asset"; then
    prebuilt_reason="release archive unavailable for $tag on $os/$arch"
    return 1
  fi
  if command -v sha256sum >/dev/null; then
    actual=$(sha256sum < "$stage/$asset") || { prebuilt_reason='checksum command failed'; return 1; }
  elif command -v shasum >/dev/null; then
    actual=$(shasum -a 256 < "$stage/$asset") || { prebuilt_reason='checksum command failed'; return 1; }
  else
    prebuilt_reason='no SHA-256 command available'
    return 1
  fi
  if [[ ${actual%% *} != "$expected" ]]; then
    prebuilt_reason='checksum mismatch'
    return 1
  fi

  # Read only the named members and never extract entries onto disk. Check
  # the archive identifies this checkout, even if a fork reuses the tag name.
  if ! archived_commit=$(tar -xOzf "$stage/$asset" COMMIT); then
    prebuilt_reason='cannot read the verified gzip archive with tar'
    return 1
  fi
  if [[ $archived_commit != "$commit" ]]; then
    prebuilt_reason='binary archive does not match this checkout'
    return 1
  fi
  if ! tar -xOzf "$stage/$asset" crosshall > "$stage/crosshall" || ! chmod 755 "$stage/crosshall"; then
    prebuilt_reason='cannot extract the verified binary'
    return 1
  fi
}

fetch() {
  curl --disable --fail --silent --location --proto '=https' --proto-redir '=https' \
    --connect-timeout 5 --max-time 60 --output "$2" "$1"
}

build_source() (
  mkdir "$stage/src"
  git -C "$repo" archive "$commit" | tar -x -C "$stage/src"
  cd "$stage/src"
  export CGO_ENABLED=0 GOCACHE="$stage/build-cache"

  # A Go download cache also serves as a read-only file proxy.
  downloads=$(build_go env GOMODCACHE)/cache/download
  if [[ -d $downloads ]]; then
    proxy=$(build_go env GOPROXY)
    url=${downloads//%/%25}
    url=${url//\#/%23}
    url=${url//\?/%3F}
    url=${url//,/%2C}
    url=${url//\|/%7C}
    export GOPROXY="file://$url,$proxy"
  fi
  export GOPATH="$stage/gopath" GOMODCACHE="$stage/modules"
  build_go build -modcacherw -mod=readonly -buildvcs=false -trimpath \
    -ldflags "-X main.version=$tag" -o "$stage/crosshall" ./cmd/crosshall
)

check_version() {
  local delay
  # Linux may briefly return exit 126 for a program just built by Go.
  for delay in 0.1 0.2 0.4 0.8 ''; do
    status=0
    version=$("$stage/crosshall" version 2>"$stage/version.err") || status=$?
    [[ $status == 126 && -n $delay ]] || break
    sleep "$delay"
  done
  [[ $status == 0 && $version == "$tag" ]]
}

prebuilt=false
if [[ $from_source == false ]]; then
  if fetch_prebuilt; then
    if check_version; then
      prebuilt=true
    else
      prebuilt_reason='verified binary did not report the release version'
    fi
  fi
  if [[ $prebuilt == false ]]; then
    printf 'Could not use a prebuilt binary (%s); falling back to a source build.\n' "$prebuilt_reason" >&2
  fi
fi
if [[ $prebuilt == false ]]; then
  if ! command -v go >/dev/null; then
    printf 'A source build needs Go %s or later on PATH.\n' \
      "$(git -C "$repo" show "$commit:go.mod" | awk '$1 == "go" { print $2; exit }')" >&2
    exit 1
  fi
  # A rejected archive may have supplied an executable with the same path.
  rm -f -- "$stage/crosshall"
  build_source
  if ! check_version; then
    if [[ $status != 0 ]]; then
      cat "$stage/version.err" >&2
      echo "The built program did not run." >&2
    else
      echo "Built version does not match $tag." >&2
    fi
    exit 1
  fi
fi
if [[ -e $stage/incumbent ]] && ! command -v go >/dev/null && ! cmp -s "$stage/incumbent" "$stage/crosshall"; then
  echo "Could not recognize the existing executable at $target without Go. Use --force to replace it." >&2
  exit 1
fi

if [[ $force == true ]]; then
  mv -f -- "$stage/crosshall" "$dest/"
elif [[ -e $stage/incumbent ]]; then
  if [[ -L $target || ! $target -ef $stage/incumbent ]]; then
    echo "Destination changed during installation at $target." >&2
    exit 1
  fi
  mv -f -- "$stage/crosshall" "$dest/"
else
  # An entry created during the build must survive a first-time install.
  ln -- "$stage/crosshall" "$dest/"
fi
printf 'Installed crosshall %s at %s\n' "$version" "$target"
if [[ ! $(command -v crosshall) -ef $target ]]; then
  printf 'export PATH=%q:"$PATH"\n' "$dest"
fi
printf 'Next (in a plain terminal): crosshall owner start\n'

[[ -t 0 && -t 1 && ! ${CI+x} ]] || exit 0
# Match ident.AllowOwner's presence check. The binary rechecks the caller
# before reading adapter status or saved choices.
for marker in CROSSHALL_ROUTE CLAUDE_CODE_SESSION_ID CODEX_THREAD_ID ANTIGRAVITY_CONVERSATION_ID \
              CLAUDECODE AI_AGENT CODEX_SESSION_ID ANTIGRAVITY_AGENT; do
  [[ ! ${!marker+x} ]] || exit 0
done
if ! "$target" setup --offer; then
  printf 'Optional adapter setup failed. Run %q setup to retry.\n' "$target" >&2
fi
