#!/usr/bin/env bash
# Install the release tag at HEAD. Local source edits do not enter the binary.
set -euo pipefail

usage='Usage: scripts/install.sh [--force] [--source] [install-directory]  (default: ~/.local/bin)'

profile_writable() {
  [[ ! -L $1 && -w ${1%/*} && ( ! -e $1 || ( -f $1 && -r $1 && -w $1 ) ) ]]
}

path_mode() {
  if [[ ! -e $1 ]]; then
    printf 'created\n'
  elif [[ -s $1 ]] && [[ $(tail -c 1 "$1" | wc -l) -eq 0 ]]; then
    printf 'separator\n'
  else
    printf 'existing\n'
  fi
}

path_block() {
  # A separator marker owns the preceding newline when the file lacked one.
  if [[ $1 == separator ]]; then printf '\n' || return; fi
  printf '# crosshall: user PATH %s\n' "$1" || return
  cat <<'PATH_BLOCK'
case "${PATH-}" in
  "$HOME/.local/bin"|"$HOME/.local/bin:"*) ;;
  *) export PATH="$HOME/.local/bin${PATH:+:$PATH}" ;;
esac
# crosshall: end user PATH
PATH_BLOCK
}

has_path_marker() {
  [[ -f $1 ]] && grep -Eq '^# crosshall: user PATH( |$)' "$1"
}

snapshot_path() {
  ln -P -- "$1" "$2/live" && [[ ! -L $2/live && -f $2/live ]] &&
    cp -p -- "$2/live" "$2/original"
}

path_unchanged() {
  [[ ! -L $1 && $1 -ef $2/live ]] && cmp -s "$1" "$2/original"
}

path_receipt() {
  local identity
  [[ ${HOME:-} == /* ]] || return 1
  case $(uname -s) in
    Darwin) identity=$(stat -f '%d-%i-%B' "$1") || return 1 ;;
    Linux) identity=$(stat -c '%d-%i-%W' "$1") || return 1 ;;
    *) return 1 ;;
  esac
  [[ $identity =~ ^[0-9]+-[0-9]+-[0-9]+$ ]] || return 1
  printf '%s/.local/bin/.crosshall-path-owners/%s\n' "$HOME" "$identity"
}

store_path_receipt() (
  local profile=$1 mode=$2 receipt=$3 dir=${3%/*}
  [[ -n $receipt && ! -L $dir ]] || return 1
  umask 077
  mkdir -p -- "$dir" && [[ ! -L $dir && -O $dir ]] || return 1
  set -C
  printf '%s\n%s' "$mode" "$profile" > "$receipt"
)

read_path_receipt() {
  local profile=$1 mode=$2 receipt dir
  receipt=$(path_receipt "$3") || return 1
  dir=${receipt%/*}
  [[ -d $dir && ! -L $dir && -O $dir && -f $receipt && ! -L $receipt && -O $receipt ]] &&
    cmp -s "$receipt" <(printf '%s\n%s' "$mode" "$profile") || return 1
  printf '%s\n' "$receipt"
}

remove_path() (
  local profile=$1 scratch range start mode length size receipt=
  if [[ $profile != /* ]]; then
    printf 'Use the absolute shell startup path: %q\n' "$profile" >&2
    return 1
  fi
  [[ -e $profile || -L $profile ]] || return 0
  if ! profile_writable "$profile"; then
    printf 'Cannot update shell startup file safely: %q\n' "$profile" >&2
    return 1
  fi
  has_path_marker "$profile" || return 0
  scratch=$(mktemp -d "${profile%/*}/.crosshall-path.XXXXXXXX") || return 1
  trap 'rm -rf -- "$scratch"' EXIT
  snapshot_path "$profile" "$scratch" || return 1
  if ! range=$(LC_ALL=C awk '
    /^# crosshall: user PATH( |$)/ { found++; start = offset + 0; mode = $NF }
    { offset += length($0) + 1 }
    END {
      if (found != 1 || (mode != "created" && mode != "existing" && mode != "separator")) exit 1
      print start, mode
    }
  ' "$scratch/original"); then
    printf 'Unrecognized PATH marker in %q. Review the file before removing it.\n' "$profile" >&2
    return 1
  fi
  read -r start mode <<< "$range"
  if [[ $mode == separator ]]; then start=$((start - 1)); fi
  length=$(path_block "$mode" | wc -c)
  if (( start < 0 )) || ! cmp -s <(path_block "$mode") \
      <(dd if="$scratch/original" bs=1 skip="$start" count="$length" 2>/dev/null); then
    printf 'Managed PATH block changed in %q. Review the file before removing it.\n' "$profile" >&2
    return 1
  fi
  size=$(wc -c < "$scratch/original")
  if [[ $mode == created || $mode == separator ]]; then
    receipt=$(read_path_receipt "$profile" "$mode" "$scratch/live") || receipt=
  fi
  # If the user added a suffix, it still needs its own line.
  if [[ $mode == separator ]] && { [[ -z $receipt ]] || (( start + length < size )); }; then
    start=$((start + 1))
    length=$((length - 1))
  fi
  cp -p -- "$scratch/original" "$scratch/updated" || return 1
  { (( start == 0 )) || head -c "$start" "$scratch/original"; } > "$scratch/updated" &&
    tail -c "+$((start + length + 1))" "$scratch/original" >> "$scratch/updated" || return 1
  if ! path_unchanged "$profile" "$scratch"; then
    printf 'Shell startup file changed during removal: %q\n' "$profile" >&2
    return 1
  fi
  if [[ $mode == created && -n $receipt ]] && (( length == size )) &&
     [[ $(LC_ALL=C ls -ld -- "$scratch/original") == '-rw-------'* ]]; then
    rm -- "$profile" || return 1
  else
    mv -f -- "$scratch/updated" "$profile" || return 1
  fi
  if [[ -n $receipt ]] && rm -- "$receipt"; then
    rmdir -- "${receipt%/*}" 2>/dev/null || true
  fi
  printf 'Removed Crosshall PATH setup from %q\n' "$profile"
)

if [[ ${1:-} == --remove-path ]]; then
  shift
  if (( $# == 0 )); then
    echo 'Usage: scripts/install.sh --remove-path <absolute-startup-file> ...' >&2
    exit 2
  fi
  result=0
  for profile in "$@"; do remove_path "$profile" || result=1; done
  exit "$result"
fi

force=false
from_source=false
dest=
for arg in "$@"; do
  case $arg in
    --help|-h)
      echo "$usage"
      echo 'Use --force to replace an unrecognized file or a symbolic link itself.'
      echo 'Use --source to skip the release download and build with Go.'
      echo 'Use --remove-path <absolute-startup-file> ... to remove the installed PATH block.'
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

default_destination=false
if [[ -z $dest ]]; then
  default_destination=true
  dest="$HOME/.local/bin"
fi
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
  local checksum_http curl_status=0
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
  checksum_http=$(fetch "$url.sha256" "$stage/checksum" --write-out '%{http_code}') || curl_status=$?
  if [[ $checksum_http == 404 ]]; then
    prebuilt_reason="no release archive is published for $tag on $os/$arch"
    return 1
  fi
  if (( curl_status != 0 )); then
    case "$curl_status" in
      5|6|7|28)
        prebuilt_reason="could not reach the release for $tag on $os/$arch" ;;
      *)
        prebuilt_reason="no verified release archive found for $tag on $os/$arch because the checksum is unavailable" ;;
    esac
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
  local url=$1 target=$2
  shift 2
  curl --disable --fail --silent --location --proto '=https' --proto-redir '=https' \
    --connect-timeout 5 --max-time 60 "$@" --output "$target" "$url"
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
owner_terminal() {
  [[ -t 0 && -t 1 && ! ${CI+x} ]] || return 1
  local marker
  # Match ident.AllowOwner's presence check. The binary rechecks its caller.
  for marker in CROSSHALL_ROUTE CLAUDE_CODE_SESSION_ID CODEX_THREAD_ID ANTIGRAVITY_CONVERSATION_ID \
                CLAUDECODE AI_AGENT CODEX_SESSION_ID ANTIGRAVITY_AGENT; do
    [[ ! ${!marker+x} ]] || return 1
  done
}

path_profiles=()
path_modes=()

plan_path() {
  local profile zhome=${ZDOTDIR:-$HOME}
  local startup_profiles=("$HOME/.profile" "$HOME/.bashrc")
  # Bash reads only the first readable login file. POSIX login shells use .profile.
  for profile in "$HOME/.bash_profile" "$HOME/.bash_login"; do
    if [[ -f $profile && -r $profile ]]; then
      startup_profiles+=("$profile")
      break
    fi
  done
  if [[ $zhome == /* && -d $zhome ]] &&
     { [[ ${SHELL:-} == */zsh ]] || command -v zsh >/dev/null; }; then
    # Login shells can reorder PATH after .zshenv, notably via macOS path_helper.
    startup_profiles+=("$zhome/.zshenv" "$zhome/.zlogin")
    if [[ -e $zhome/.zshrc || -L $zhome/.zshrc ]]; then startup_profiles+=("$zhome/.zshrc"); fi
  fi
  for profile in "${startup_profiles[@]}"; do
    if ! profile_writable "$profile"; then
      printf 'Skipping shell startup file that cannot be updated safely: %q\n' "$profile" >&2
    elif ! has_path_marker "$profile"; then
      path_profiles+=("$profile")
      path_modes+=("$(path_mode "$profile")")
    fi
  done
}

write_path_block() (
  local profile=$1 mode=$2 scratch receipt=
  scratch=$(mktemp -d "${profile%/*}/.crosshall-path.XXXXXXXX") || return 1
  trap 'rm -rf -- "$scratch"' EXIT
  if [[ $mode == created ]]; then
    (umask 077; path_block "$mode" > "$scratch/updated") || return 1
  else
    snapshot_path "$profile" "$scratch" &&
      [[ $(path_mode "$scratch/original") == "$mode" ]] &&
      ! has_path_marker "$scratch/original" &&
      cp -p -- "$scratch/original" "$scratch/updated" &&
      path_block "$mode" >> "$scratch/updated" || return 1
  fi
  if [[ $mode == created || $mode == separator ]]; then
    receipt=$(path_receipt "$scratch/updated") || receipt=
  fi
  if [[ $mode == created ]]; then
    ln -- "$scratch/updated" "$profile" || return 1
  else
    path_unchanged "$profile" "$scratch" &&
      mv -f -- "$scratch/updated" "$profile" || return 1
  fi
  if [[ $mode == created || $mode == separator ]] && ! store_path_receipt "$profile" "$mode" "$receipt"; then
    printf 'Could not record PATH setup ownership for %q. Removal will keep the file and any uncertain separator.\n' "$profile" >&2
  fi
)

apply_path() {
  local i profile mode
  for i in "${!path_profiles[@]}"; do
    profile=${path_profiles[i]}
    mode=${path_modes[i]}
    if ! profile_writable "$profile" || [[ $(path_mode "$profile") != "$mode" ]] || has_path_marker "$profile"; then
      printf 'Shell startup file changed before it could be updated: %q\n' "$profile" >&2
    elif write_path_block "$profile" "$mode"; then
      printf 'Updated PATH in %q\n' "$profile"
    else
      printf 'Could not update shell startup file: %q\n' "$profile" >&2
    fi
  done
}

print_next() {
  if [[ $on_path == true ]]; then
    printf 'Next (in a plain terminal): crosshall owner start\n'
  else
    printf 'Next (in a plain terminal): %q owner start\n' "$target"
  fi
}

offer_setup() {
  # Keep both streams attached to the terminal for the consent prompt.
  if ! CROSSHALL_INSTALL_PATH_CONSENT="$1" CROSSHALL_INSTALL_READY_RECEIPT=1 \
      "$target" setup --offer 3> "$stage/path-choice" 4> "$stage/setup-ready"; then
    printf 'Optional adapter setup failed. Run %q setup to retry.\n' "$target" >&2
  fi
}

printf 'Installed crosshall %s at %s\n' "$version" "$target"
on_path=false
if [[ $(command -v crosshall) -ef $target ]]; then on_path=true; fi
if ! owner_terminal; then
  print_next
  exit 0
fi
if [[ $default_destination == true && $on_path == false && $HOME == /* && $HOME != *:* ]]; then
  plan_path
fi
if [[ ${path_profiles[0]+planned} ]]; then
  printf 'Add ~/.local/bin first on PATH with the following shell startup changes:\n'
  for i in "${!path_profiles[@]}"; do
    printf '\n  %q\n' "${path_profiles[i]}"
    path_block "${path_modes[i]}"
  done
  offer_setup 1
  answer=$(cat "$stage/path-choice")
  if [[ -z $answer ]]; then
    printf 'Apply the PATH changes shown? [y/N] '
    read -r answer || answer=
  fi
  case $answer in [yY]|[yY][eE][sS]) apply_path ;; esac
else
  offer_setup 0
fi
if ! grep -qsFx ready "$stage/setup-ready"; then
  print_next
fi
