#!/usr/bin/env bash
# Build the four release artifacts from one committed tree.
set -euo pipefail
export LC_ALL=C

if [[ $# != 2 ]]; then
  echo 'Usage: scripts/release.sh <release-tag|dev> <output-directory>' >&2
  exit 2
fi

version=$1
out=$2
repo=$(cd -- "$(dirname -- "$0")/.." && pwd -P)
commit=$(git -C "$repo" rev-parse --verify HEAD)
release='^v[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.+-]+)?$'
if [[ $version != dev ]]; then
  if ! [[ $version =~ $release ]] ||
      [[ $(git -C "$repo" rev-parse --verify "refs/tags/$version^{commit}") != "$commit" ]]; then
    echo "Check out release tag $version before building its artifacts." >&2
    exit 1
  fi
fi

mkdir -p -- "$out"
out=$(cd -- "$out" && pwd -P)
stage=$(mktemp -d "${TMPDIR:-/tmp}/crosshall-release.XXXXXXXX")
trap 'rm -rf -- "$stage"' EXIT
mkdir "$stage/src" "$stage/out" "$stage/telemetry"
printf '%s\n' "$commit" > "$stage/COMMIT"
printf 'off\n' > "$stage/telemetry/mode"
git -C "$repo" archive "$commit" | tar -x -C "$stage/src"

export GOENV=off GOFLAGS='' GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=0 GOEXPERIMENT=''
export TEST_TELEMETRY_DIR="$stage/telemetry"
unset GOOS GOARCH
go_build_version=$(awk '$1 == "go" { print "go" $2; exit }' "$stage/src/go.mod")
if [[ $(go env GOVERSION) != "$go_build_version" ]]; then
  printf 'Release builds require %s.\n' "$go_build_version" >&2
  exit 1
fi

# Build and package the archived tree, including its audited notices.
cd "$stage/src"
inventory=scripts/release-notices/modules.txt
for platform in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
  os=${platform%/*}
  arch=${platform#*/}
  asset="crosshall-$os-$arch.tar.gz"
  printf 'Building %s for %s\n' "$version" "$platform"
  GOOS=$os GOARCH=$arch GOAMD64=v1 GOARM64=v8.0 go build \
    -mod=readonly -buildvcs=false -trimpath -pgo=off -ldflags "-X main.version=$version -buildid=" \
    -o "$stage/crosshall" ./cmd/crosshall

  # Compare the audited inventory with the resulting binary, not the larger
  # module graph, which also includes dependencies of tools and tests.
  go version -m "$stage/crosshall" | awk '$1 == "dep" {print $2, $3}' | sort > "$stage/linked"
  awk -v target="$os" '$3 == "all" || $3 == target {print $1, $2}' "$inventory" | sort > "$stage/audited"
  if ! diff -u "$stage/audited" "$stage/linked"; then
    echo "Review the third-party notices for $platform before releasing." >&2
    exit 1
  fi

  {
    printf 'Third-party notices for crosshall %s (%s)\n' "$version" "$platform"
    for notice in go-LICENSE go-PATENTS go-x-net-LICENSE go-x-net-PATENTS; do
      printf '\n--- %s ---\n\n' "$notice"
      cat "scripts/release-notices/$notice"
    done
    while read -r module revision target notices; do
      [[ $target == all || $target == "$os" ]] || continue
      module_dir=$(go list -m -f '{{.Dir}}' "$module")
      for notice in $notices; do
        printf '\n--- %s %s / %s ---\n\n' "$module" "$revision" "$notice"
        cat "$module_dir/$notice"
      done
    done < "$inventory"
  } > "$stage/THIRD_PARTY_NOTICES.txt"
  go run scripts/release_archive.go "$stage/out/$asset" "$stage/crosshall" LICENSE NOTICE \
    "$stage/THIRD_PARTY_NOTICES.txt" "$stage/COMMIT"
done
cp -- "$stage/out/"* "$out/"
