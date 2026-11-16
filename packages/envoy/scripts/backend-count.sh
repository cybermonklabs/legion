#!/usr/bin/env bash
# packages/envoy/scripts/backend-count.sh
#
# Counts, for each test it names, the runs that passed on each store backend: the test's own
# `aws` and `gcp` subtests (onEachStore, cmd/agent-secrets/secret_kind_test.go), so a store or
# use change cannot pass on one backend alone. It runs, from packages/envoy, only the named tests:
#
#   go test -json -count=1 -timeout "${GO_TEST_TIMEOUT:-5m}" -run '^(TEST|...)$' [-skip "$GO_TEST_SKIP"] ./cmd/agent-secrets/
#
# and prints one line per named test, `TEST aws=N gcp=M`, N and M counting the `pass` events of
# `TEST/aws` and `TEST/gcp`, then `go_test_exit=E`. It counts only the tests it is given, so the
# `aws` and `gcp` subtests of other tests cannot stand in for a named test's. The database broker
# suites' skips are database-skips.sh's count, not this one's.
#
# It exits 1 when no test is named, when a name is not a Go test name, when a named test has no
# passing run on either backend, or when go test failed; 0 otherwise. GO_TEST_SKIP is go test's
# -skip pattern, which a negative control sets to take the `aws` subtests away
# ('^(TestA|TestB)$/^aws$'). GO_TEST_TIMEOUT is go test's -timeout, 5m unless set: the named tests
# take seconds, and CI's job budget counts this step at that bound.
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "backend-count.sh: name at least one test whose aws and gcp subtests to count" >&2
  exit 1
fi
for test in "$@"; do
  if ! [[ "$test" =~ ^Test[A-Za-z0-9_]*$ ]]; then
    echo "backend-count.sh: $test is not a Go test name" >&2
    exit 1
  fi
done
run="^($(IFS='|'; echo "$*"))\$"

cd "$(dirname "$0")/.."
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
events="$work/events.json"

go_test_exit=0
go test -json -count=1 -timeout "${GO_TEST_TIMEOUT:-5m}" -run "$run" ${GO_TEST_SKIP:+-skip "$GO_TEST_SKIP"} ./cmd/agent-secrets/ >"$events" 2>"$work/stderr" || go_test_exit=$?
if [ ! -s "$events" ]; then
  echo "backend-count.sh: go test wrote no events (exit $go_test_exit), so nothing was counted:" >&2
  cat "$work/stderr" >&2
  exit 1
fi

status=0
for test in "$@"; do
  counts=()
  for backend in aws gcp; do
    n="$(jq -r --arg name "$test/$backend" 'select(.Action == "pass" and .Test == $name) | .Test' "$events" | wc -l)"
    counts+=("$backend=$n")
    [ "$n" -gt 0 ] || status=1
  done
  echo "$test ${counts[*]}"
done

echo "go_test_exit=$go_test_exit"
if [ "$go_test_exit" -ne 0 ]; then
  status=1
  jq -r 'select(.Action == "fail" and .Test != null) | "FAIL \(.Package) \(.Test)"' "$events" >&2
  jq -rsj '[.[] | select(.Action == "fail" and .Test != null) | .Test] as $failed | .[] | select(.Action == "output" and .Test != null) | select(.Test as $t | any($failed[]; . == $t)) | .Output' "$events" >&2
  cat "$work/stderr" >&2
fi
exit "$status"
