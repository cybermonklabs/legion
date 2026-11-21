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
# changes to packages/envoy. It fetches the modules <package> loads first, retried, and exits with
# the retry's status when every attempt failed, with go build's otherwise, and with 2 and the
# usage for any other number of arguments.
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
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/ci-retry.sh
. "$script_dir/ci-retry.sh"

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
# The build reads its modules from the action's own GOMODCACHE, which a miss of the action's cache
# leaves empty, and a caller's own retried `go mod download` fills another cache, so the build
# would fetch the modules it loads once and unretried. Those modules are fetched first, under the
# same settings, through the one retry helper at the module bound (ci-retry.sh beside this script,
# which GitHub fetches whole for a caller in another repository too), and the build then fetches
# nothing. `go list -deps` of the package is that fetch, and `go mod download` is not: with no
# argument it fetches every module packages/envoy/go.mod requires, where each command loads a
# few. Measured on an empty cache: for ./cmd/architecture-check the list fetched two modules
# (gopkg.in/yaml.v3 and github.com/pmezard/go-difflib), 684 KB in 6 s, and the build after it
# succeeded with GOPROXY=off (dispatch://INFRA-1545, review round 28); for ./cmd/spec-check it
# fetched six (those two, github.com/google/uuid, github.com/yuin/goldmark, golang.org/x/text and
# github.com/sjawhar/ygo, which replaces github.com/reearth/ygo), 12,590 KB in 9 s, and the build
# after it, with an empty build cache too, succeeded with GOPROXY=off in 38 s, both in a 2-CPU pod
# (dispatch://INFRA-1545, the merge of main at ea2f5f93). Main's #3994 added this build and #4172
# moved it here; dispatch://INFRA-1545 retried its fetch; dispatch://INFRA-2648 owns teaching
# check-download-retry.sh to read a private module cache. The script runs as a process of its
# own, so the settings are exported for the two commands and reach no caller.
unset GOOS GOARCH
export GOWORK=off GOTOOLCHAIN=local GOFLAGS= GOENV=off
export GOMODCACHE="$module_cache" GOCACHE="$build_cache"
retry_fetch "the Go modules of $package" 180 -- go list -deps "$package" >/dev/null
go build -buildvcs=false -o "$binary" "$package"
