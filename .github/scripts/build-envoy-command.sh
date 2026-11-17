#!/usr/bin/env bash
# The one build of the commands of the composite actions .github/actions/architecture-check and
# .github/actions/spec-check: each one's `check.sh build` runs it as
#
#   bash "$GITHUB_ACTION_PATH/../../scripts/build-envoy-command.sh" \
#     <package> <binary> <module cache> <build cache>
#
# The runner fetches this whole repository for an action that another repository calls, so this
# script is there beside the action, and it finds packages/envoy from its own path.
#
# It builds <package> of packages/envoy, such as ./cmd/spec-check, into <binary>, with its module
# cache in <module cache> and its build cache in <build cache>, the two directories the action's
# cache steps restore and save, and it makes the three directories first. A relative <binary>,
# <module cache> or <build cache> is read against the directory the script runs in, before it
# changes to packages/envoy. It exits with go build's status, or 2 with the usage for any other
# number of arguments.
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo 'usage: build-envoy-command.sh <package> <binary> <module cache> <build cache>' >&2
  exit 2
fi
# absolute prints its argument as an absolute path, read against the directory the script runs in.
absolute() {
  case $1 in
    /*) printf '%s\n' "$1" ;;
    *) printf '%s\n' "$PWD/$1" ;;
  esac
}
package=$1
binary=$(absolute "$2") module_cache=$(absolute "$3") build_cache=$(absolute "$4")
module="$(dirname "${BASH_SOURCE[0]}")/../../packages/envoy"

mkdir -p "$module_cache" "$build_cache" "$(dirname "$binary")"
cd "$module"
# Each setting keeps the caller's job out of this build:
# - GOWORK=off: the repository's root go.work sits above the action's directory, and the build
#   reads packages/envoy/go.mod alone.
# - GOTOOLCHAIN=local: a Go older than go.mod's go line fails by name, and the go command fetches
#   no other toolchain in its place.
# - GOFLAGS empty and GOENV=off: the go command reads an empty GOFLAGS from the user's
#   `go env -w` file instead, so the variable alone does not clear a flag a runner image set.
# - GOOS and GOARCH unset: a job that cross-compiles its own code cannot make a binary this runner
#   cannot run.
# - GOMODCACHE and GOCACHE: the action's own caches, so an entry holds this build alone and never
#   the caller's.
env -u GOOS -u GOARCH \
  GOWORK=off GOTOOLCHAIN=local GOFLAGS= GOENV=off \
  GOMODCACHE="$module_cache" GOCACHE="$build_cache" \
  go build -buildvcs=false -o "$binary" "$package"
