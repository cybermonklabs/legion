#!/usr/bin/env bash
# packages/envoy/scripts/backend-count.sh
#
# Counts, for each test it names, the runs that passed on each store backend: the test's own
# `aws` and `gcp` subtests (onEachStore, cmd/agent-secrets/secret_kind_test.go), so a store or
# use change cannot pass on one backend alone. It runs, from packages/envoy,
#
#   go test -json -count=1 [-skip "$GO_TEST_SKIP"] ./cmd/agent-secrets/ ./internal/broker/...
#
# and prints one line per named test, `TEST aws=N gcp=M`, N and M counting the `pass` events of
# `TEST/aws` and `TEST/gcp`, then `database_skips=K`, the tests that ended `skip` and whose output
# names BROKER_TEST_DATABASE_URL, and `go_test_exit=E`. It counts only the tests it is given, so
# the `aws` and `gcp` subtests of other tests cannot stand in for a named test's.
#
# It exits 1 when no test is named, when a named test has no passing run on either backend, when
# a database suite skipped, or when go test failed; 0 otherwise. GO_TEST_SKIP is go test's -skip
# pattern, which a negative control sets to take the `aws` subtests away
# ('^(TestA|TestB)$/^aws$'); with BROKER_TEST_DATABASE_URL unset, the database suites skip and
# the count says so.
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "backend-count.sh: name at least one test whose aws and gcp subtests to count" >&2
  exit 1
fi

cd "$(dirname "$0")/.."
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
events="$work/events.json"

go_test_exit=0
go test -json -count=1 ${GO_TEST_SKIP:+-skip "$GO_TEST_SKIP"} ./cmd/agent-secrets/ ./internal/broker/... >"$events" 2>"$work/stderr" || go_test_exit=$?
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

jq -r 'select(.Action == "skip" and .Test != null) | "\(.Package) \(.Test)"' "$events" | sort -u >"$work/skipped"
jq -r 'select(.Action == "output" and .Test != null and (.Output | contains("BROKER_TEST_DATABASE_URL"))) | "\(.Package) \(.Test)"' "$events" | sort -u >"$work/named"
database_skips="$(comm -12 "$work/skipped" "$work/named" | wc -l)"
echo "database_skips=$database_skips"
[ "$database_skips" -eq 0 ] || status=1

echo "go_test_exit=$go_test_exit"
if [ "$go_test_exit" -ne 0 ]; then
  status=1
  jq -r 'select(.Action == "fail" and .Test != null) | "FAIL \(.Package) \(.Test)"' "$events" >&2
  cat "$work/stderr" >&2
fi
exit "$status"
