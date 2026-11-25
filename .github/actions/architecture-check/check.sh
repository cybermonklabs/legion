#!/usr/bin/env bash
# The script of the composite action beside it (action.yml): it builds packages/envoy's
# architecture-check command from the checkout of this repository the runner fetched for the
# action, and runs it over a notes directory of the caller's checkout. The action runs it as
# `bash "$GITHUB_ACTION_PATH/check.sh" <mode>`, so its file mode does not matter, and a person can
# run it by hand the same way, with GITHUB_WORKSPACE pointed at any tree.
#
# Usage: check.sh build | check.sh run | check.sh go-path
#
#   build    Builds ./cmd/architecture-check of $GITHUB_ACTION_PATH/../../../packages/envoy into
#            $RUNNER_TEMP/architecture-check/bin/architecture-check, with its module cache in
#            $RUNNER_TEMP/architecture-check/mod and its build cache in
#            $RUNNER_TEMP/architecture-check/build, the two directories the action's cache steps
#            restore and save. The build is .github/scripts/build-envoy-command.sh, the one build
#            of both composite actions of this repository, which holds the settings that keep the
#            caller's job out of it. It fetches the modules the command loads first, retried,
#            and exits with the retry's status when every attempt failed, and with go build's
#            otherwise.
#   run      Runs that binary over ARCHITECTURE_CHECK_PATH, resolved against GITHUB_WORKSPACE (an
#            absolute path stays as it is), with --allow-empty when ARCHITECTURE_CHECK_ALLOW_EMPTY
#            is `true`. The binary's standard error goes to the log, and its standard output to
#            $RUNNER_TEMP/architecture-check/stdout, which this script then prints. When that
#            output holds the count line, `architecture-check: <dir>: <n> notes, the set is valid`
#            or `... the set is refused` (`note` for one), it appends `count=<n>` to GITHUB_OUTPUT.
#            It exits with the binary's status: 0 valid, 1 refused, 2 not judged (no count line
#            then).
#   go-path  Checks that the Go on PATH is the release packages/envoy/go.mod names. It reads the
#            toolchain line of go.mod if present, else the go line, and compares with the output
#            of `GOTOOLCHAIN=local GOWORK=off GOENV=off go env GOVERSION`, the same settings
#            build-envoy-command.sh uses, so the caller's own GOTOOLCHAIN, go env -w file or
#            workspace cannot change the version. It refuses a go line without a patch number
#            (such as `go 1.27`). When the two differ, it exits 2 with a line naming both
#            releases and saying to install go.mod's release before the action.
#
# build and run need GITHUB_ACTION_PATH and RUNNER_TEMP. run also needs GITHUB_WORKSPACE,
# GITHUB_OUTPUT, ARCHITECTURE_CHECK_PATH and ARCHITECTURE_CHECK_ALLOW_EMPTY, which is exactly
# `true` or `false`. go-path needs GITHUB_ACTION_PATH only. A missing variable, an
# ARCHITECTURE_CHECK_ALLOW_EMPTY of any other value, a run before a build, or a mode other than
# the three is exit 2, with one line on standard error.
set -euo pipefail

usage='usage: check.sh build|run|go-path'

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

case "$#:${1:-}" in
  1:build | 1:run | 1:go-path) mode=$1 ;;
  *)
    echo "$usage" >&2
    exit 2
    ;;
esac

require GITHUB_ACTION_PATH

build() {
  bash "$GITHUB_ACTION_PATH/../../scripts/build-envoy-command.sh" \
    ./cmd/architecture-check "$binary" "$work/mod" "$work/build"
}

run() {
  require GITHUB_WORKSPACE GITHUB_OUTPUT ARCHITECTURE_CHECK_PATH ARCHITECTURE_CHECK_ALLOW_EMPTY
  case "$ARCHITECTURE_CHECK_PATH" in
    /*) dir=$ARCHITECTURE_CHECK_PATH ;;
    *) dir="$GITHUB_WORKSPACE/$ARCHITECTURE_CHECK_PATH" ;;
  esac
  case "$ARCHITECTURE_CHECK_ALLOW_EMPTY" in
    true) set -- --allow-empty "$dir" ;;
    false) set -- "$dir" ;;
    *) refuse "the input allow-empty must be 'true' or 'false'" ;;
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

# go_path checks that the Go on PATH is the release packages/envoy/go.mod names. The action's
# go-path mode (its input `go` set to `path`) runs it before the build, so a caller whose Go differs
# from go.mod's release sees a refusal naming both releases instead of a build failure.
go_path() {
  local gomod="$GITHUB_ACTION_PATH/../../../packages/envoy/go.mod"
  [ -r "$gomod" ] || refuse "packages/envoy/go.mod not found"

  # Read the release packages/envoy/go.mod names: the toolchain line if present, else the go
  # line. The toolchain line is `toolchain go1.27.3` and the go line is `go 1.27.3`, compared
  # with `go env GOVERSION`'s `go1.27.3` form.
  local want="" line
  while IFS= read -r line; do
    case "$line" in
      'toolchain go'[0-9]*)
        want="${line#toolchain }"
        break
        ;;
    esac
  done < "$gomod"
  if [ -z "$want" ]; then
    while IFS= read -r line; do
      case "$line" in
        'go '[0-9]*)
          want="go${line#go }"
          break
          ;;
      esac
    done < "$gomod"
  fi
  [ -n "$want" ] || refuse "could not read the go or toolchain line from packages/envoy/go.mod"

  # Path mode needs a full release (go1.N.N): a go line without a patch number such as `go 1.27`
  # gives go1.27, which no release is named.
  case "$want" in
    go[0-9]*.[0-9]*.[0-9]*) ;;
    *) refuse "packages/envoy/go.mod names $want, which is not a full release (such as go1.27.0): path mode needs a full release" ;;
  esac

  # Read with the build's own settings (GOTOOLCHAIN=local GOWORK=off GOENV=off), the same ones
  # build-envoy-command.sh uses, so the caller's own GOTOOLCHAIN, go env -w file or workspace
  # cannot change the version and GOTOOLCHAIN=auto cannot download an unchecked toolchain.
  local got
  got=$(GOTOOLCHAIN=local GOWORK=off GOENV=off go env GOVERSION) || refuse "go env GOVERSION failed"

  if [ "$got" != "$want" ]; then
    refuse "the Go on PATH is $got and packages/envoy/go.mod names $want: install $want before the action"
  fi
}

case "$mode" in
  go-path) go_path ;;
  *)
    require RUNNER_TEMP
    work="$RUNNER_TEMP/architecture-check"
    binary="$work/bin/architecture-check"
    "$mode"
    ;;
esac
