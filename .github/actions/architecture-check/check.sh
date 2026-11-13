#!/usr/bin/env bash
# The script of the composite action beside it (action.yml): it builds packages/envoy's
# architecture-check command from the checkout of this repository the runner fetched for the
# action, and runs it over a notes directory of the caller's checkout. The action runs it as
# `bash "$GITHUB_ACTION_PATH/check.sh" <mode>`, so its file mode does not matter, and a test runs
# it the same way with GITHUB_WORKSPACE pointed at any tree.
#
# Usage: check.sh build | check.sh run
#
#   build  Builds ./cmd/architecture-check of $GITHUB_ACTION_PATH/../../../packages/envoy into
#          $RUNNER_TEMP/architecture-check/bin/architecture-check, with its module cache in
#          $RUNNER_TEMP/architecture-check/mod and its build cache in
#          $RUNNER_TEMP/architecture-check/build, the two directories the action's cache steps
#          restore and save. It exits with go build's status.
#   run    Runs that binary over INPUT_PATH, resolved against GITHUB_WORKSPACE (an absolute path
#          stays as it is), with --allow-empty when INPUT_ALLOW_EMPTY is `true`. The binary's
#          standard error goes to the log, and its standard output to
#          $RUNNER_TEMP/architecture-check/stdout, which this script then prints. When that output
#          holds the count line, `architecture-check: <dir>: <n> notes, the set is valid` or
#          `... the set is refused` (`note` for one), it appends `count=<n>` to GITHUB_OUTPUT. It
#          exits with the binary's status: 0 valid, 1 refused, 2 not judged (no count line then).
#
# Both modes need GITHUB_ACTION_PATH and RUNNER_TEMP. run also needs GITHUB_WORKSPACE,
# GITHUB_OUTPUT, INPUT_PATH and INPUT_ALLOW_EMPTY, which is exactly `true` or `false`. A missing
# variable, an INPUT_ALLOW_EMPTY of any other value, a run before a build, or a mode other than
# the two is exit 2, with one line on standard error.
set -euo pipefail

usage='usage: check.sh build|run'

# refuse prints one line on standard error and exits 2, the status of a check that did not run.
refuse() {
  echo "architecture-check action: $*" >&2
  exit 2
}

# require refuses unless each variable it names is set and not empty.
require() {
  local name
  for name in "$@"; do
    [ -n "${!name:-}" ] || refuse "$name is not set"
  done
}

if [ "$#" -ne 1 ]; then
  echo "$usage" >&2
  exit 2
fi
mode=$1
case "$mode" in
  build | run) ;;
  *)
    echo "$usage" >&2
    exit 2
    ;;
esac

require GITHUB_ACTION_PATH RUNNER_TEMP
module="$GITHUB_ACTION_PATH/../../../packages/envoy"
work="$RUNNER_TEMP/architecture-check"
binary="$work/bin/architecture-check"

build() {
  mkdir -p "$work/mod" "$work/build" "$work/bin"
  cd "$module"
  # Each setting keeps the caller's job out of this build:
  # - GOWORK=off: the repository's root go.work sits above the action's directory, and the build
  #   reads packages/envoy/go.mod alone.
  # - GOTOOLCHAIN=local: a Go older than go.mod's go line fails by name, and the go command
  #   fetches no other toolchain in its place.
  # - GOFLAGS empty and GOENV=off: the go command reads an empty GOFLAGS from the user's
  #   `go env -w` file instead, so the variable alone does not clear a flag a runner image set.
  # - GOOS and GOARCH unset: a job that cross-compiles its own code cannot make a binary this
  #   runner cannot run.
  # - GOMODCACHE and GOCACHE: the action's own caches, so an entry holds this build alone and
  #   never the caller's.
  env -u GOOS -u GOARCH \
    GOWORK=off GOTOOLCHAIN=local GOFLAGS= GOENV=off \
    GOMODCACHE="$work/mod" GOCACHE="$work/build" \
    go build -buildvcs=false -o "$binary" ./cmd/architecture-check
}

run() {
  require GITHUB_WORKSPACE GITHUB_OUTPUT INPUT_PATH INPUT_ALLOW_EMPTY
  case "$INPUT_PATH" in
    /*) dir=$INPUT_PATH ;;
    *) dir="$GITHUB_WORKSPACE/$INPUT_PATH" ;;
  esac
  case "$INPUT_ALLOW_EMPTY" in
    true) set -- --allow-empty "$dir" ;;
    false) set -- "$dir" ;;
    *) refuse "the input allow-empty is '$INPUT_ALLOW_EMPTY', and it must be 'true' or 'false'" ;;
  esac
  [ -x "$binary" ] || refuse "no command at $binary: run check.sh build first"

  local stdout="$work/stdout" status=0 line
  "$binary" "$@" > "$stdout" || status=$?
  cat "$stdout"
  local count_line='^architecture-check: .*: ([0-9]+) notes?, the set is (valid|refused)$'
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ $line =~ $count_line ]]; then
      printf 'count=%s\n' "${BASH_REMATCH[1]}" >> "$GITHUB_OUTPUT"
      break
    fi
  done < "$stdout"
  exit "$status"
}

"$mode"
