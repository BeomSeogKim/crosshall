#!/usr/bin/env bash
# Install from the release tag at HEAD. Local source edits do not enter the binary.
set -euo pipefail

usage='Usage: scripts/install.sh [--force] [install-directory]  (default: ~/.local/bin)'
force=false
dest=
for arg in "$@"; do
  case $arg in
    --help|-h)
      echo "$usage"
      echo 'Use --force to replace an unrecognized file or a symbolic link itself.'
      exit 0 ;;
    --force) force=true ;;
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

if [[ $force == false && ( -L $target || -e $target ) ]]; then
  # Inspect and query the same inode. Older crosshall releases print only
  # the tag, so also require their Go main package before executing them.
  ln -P -- "$target" "$stage/incumbent"
  if [[ -L $stage/incumbent || ! -f $stage/incumbent || ! -x $stage/incumbent ]] ||
      ! build_go version -m "$stage/incumbent" | grep -Fx $'\tpath\tgithub.com/BeomSeogKim/crosshall/cmd/crosshall' >/dev/null ||
      ! installed=$("$stage/incumbent" version) || ! [[ $installed == dev || $installed =~ $release ]]; then
    echo "Unrecognized entry at $target. Use --force to replace it." >&2
    exit 1
  fi
fi

mkdir "$stage/src"
git -C "$repo" archive "$commit" | tar -x -C "$stage/src"
(
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
    url=${url//|/%7C}
    export GOPROXY="file://$url,$proxy"
  fi
  export GOPATH="$stage/gopath" GOMODCACHE="$stage/modules"
  build_go build -modcacherw -mod=readonly -buildvcs=false -trimpath \
    -ldflags "-X main.version=$tag" -o "$stage/crosshall" ./cmd/crosshall
)

# On Linux a binary just written by `go build -o` can briefly refuse to run
# ("text file busy", exit 126) while a process the go command forked still
# holds it open (https://go.dev/issue/22315). Retry briefly, as go does.
for delay in 0.1 0.2 0.4 0.8 ''; do
  status=0
  version=$("$stage/crosshall" version 2>"$stage/version.err") || status=$?
  [[ $status == 126 && -n $delay ]] || break
  sleep "$delay"
done
if [[ $status != 0 ]]; then
  cat "$stage/version.err" >&2
  echo "The built program did not run." >&2
  exit 1
fi
if [[ $version != "$tag" ]]; then
  echo "Built version does not match $tag." >&2
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
