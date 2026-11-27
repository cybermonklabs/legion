#!/usr/bin/env bash
# packages/envoy/scripts/named-tests.sh
#
# Runs, from packages/envoy, only the tests it names in only the packages it names, and fails
# unless each named test ran and passed. It takes `--timeout D` first if at all, then the packages,
# then `--`, then the tests:
#
#   go test -json -count=1 -timeout "${D:-15m}" -run '^(TEST|...)$' PACKAGE...
#
# and prints one line per named test, `TEST runs=R passes=P`, R and P counting the top-level `run`
# and `pass` events of TEST in any of the packages, then `go_test_exit=E`. A `-run` that matches
# nothing is a pass to go test, so a renamed or deleted test would leave a step that runs fewer
# tests than it names green; here it reads runs=0 and fails, naming the test.
#
# It exits 1 when no package or no test is named, when a package is not a ./ path, when a name is
# not a Go test name, when a named test did not run or did not pass, when go test wrote no events,
# or when go test failed; 0 otherwise.
set -euo pipefail

usage="usage: named-tests.sh [--timeout D] PACKAGE... -- TEST..."
timeout=15m
if [ "${1:-}" = "--timeout" ]; then
  if [ "$#" -lt 2 ] || ! [[ "$2" =~ ^[0-9]+[smh]$ ]]; then
    echo "named-tests.sh: --timeout takes a duration such as 15m; $usage" >&2
    exit 1
  fi
  timeout=$2
  shift 2
fi
packages=()
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
  if ! [[ "$1" =~ ^\./[A-Za-z0-9_./-]*$ ]]; then
    echo "named-tests.sh: $1 is not a package path under packages/envoy (./cmd/..., ./internal/...); $usage" >&2
    exit 1
  fi
  packages+=("$1")
  shift
done
if [ "${#packages[@]}" -eq 0 ] || [ "$#" -eq 0 ]; then
  echo "named-tests.sh: name the packages to run, then --; $usage" >&2
  exit 1
fi
shift
if [ "$#" -eq 0 ]; then
  echo "named-tests.sh: name at least one test to run; $usage" >&2
  exit 1
fi
for test in "$@"; do
  if ! [[ "$test" =~ ^Test[A-Za-z0-9_]*$ ]]; then
    echo "named-tests.sh: $test is not a Go test name" >&2
    exit 1
  fi
done
run="^($(IFS='|'; echo "$*"))\$"

cd "$(dirname "$0")/.."
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
events="$work/events.json"

go_test_exit=0
go test -json -count=1 -timeout "$timeout" -run "$run" "${packages[@]}" >"$events" 2>"$work/stderr" || go_test_exit=$?
if [ ! -s "$events" ]; then
  echo "named-tests.sh: go test wrote no events (exit $go_test_exit), so no test was seen to run:" >&2
  cat "$work/stderr" >&2
  exit 1
fi

status=0
for test in "$@"; do
  runs="$(jq -r --arg name "$test" 'select(.Action == "run" and .Test == $name) | .Test' "$events" | wc -l)"
  passes="$(jq -r --arg name "$test" 'select(.Action == "pass" and .Test == $name) | .Test' "$events" | wc -l)"
  echo "$test runs=$runs passes=$passes"
  if [ "$runs" -eq 0 ]; then
    echo "named-tests.sh: $test did not run: no test of that name is in ${packages[*]}" >&2
    status=1
  elif [ "$passes" -eq 0 ]; then
    echo "named-tests.sh: $test ran and did not pass" >&2
    status=1
  fi
done

echo "go_test_exit=$go_test_exit"
if [ "$go_test_exit" -ne 0 ]; then
  status=1
  jq -r 'select(.Action == "fail" and .Test != null) | "FAIL \(.Package) \(.Test)"' "$events" >&2
  jq -rsj '[.[] | select(.Action == "fail" and .Test != null) | .Test] as $failed | .[] | select(.Action == "output" and .Test != null) | select(.Test as $t | any($failed[]; . == $t)) | .Output' "$events" >&2
  cat "$work/stderr" >&2
fi
exit "$status"
