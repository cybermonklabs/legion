#!/usr/bin/env bash
# packages/envoy/scripts/store-live-check.sh
#
# Runs one secret store's live contract tests against that store's real service and counts what
# they did: `store-live-check.sh <store>` with <store> the store's registry id. It prints
# `passed=N skipped=M failed=K`, the name of each skipped or failed test, how many cases of each of
# the store's two live contracts passed (the store contract's reads and the writer contract's
# writes), and where the event stream is kept. It exits 0 only when N is above zero, M and K are
# both zero, and each live contract test passed with its cases.
#
# A skip counts as a failure because a live test skips when its variables are unset, which is the
# shape a live run fails in: the tests pass, nothing ran, and the run looks green. A run that
# wrote no event at all prints UNKNOWN and exits 1 rather than reading an empty set as a pass, and
# so does a run whose passing tests leave out either live contract: a passing legacy test is no
# contract run, and the read contract alone says nothing of the store's writes.
#
# Each store's tests and the variables they need:
#
#   gcp  -tags gcplive ./internal/cipher/secretstore/gcp/
#        BROKER_GCP_LIVE_PROJECT, BROKER_GCP_LIVE_KMS_KEY
#
# Run it from packages/envoy, with the store's variables in the environment:
#
#   BROKER_GCP_LIVE_PROJECT=… BROKER_GCP_LIVE_KMS_KEY=… scripts/store-live-check.sh gcp
set -euo pipefail

usage() {
  echo "usage: store-live-check.sh <store>; stores: gcp" >&2
  exit 2
}

[ $# -eq 1 ] || usage
store="$1"

case "$store" in
  gcp)
    tag=gcplive
    packages=(./internal/cipher/secretstore/gcp/)
    variables=(BROKER_GCP_LIVE_PROJECT BROKER_GCP_LIVE_KMS_KEY)
    contracts=(TestGCPLiveRunsTheStoreContract TestGCPLiveRunsTheWriterContract)
    ;;
  *)
    echo "store-live-check: no live tests for the store $store" >&2
    usage
    ;;
esac

# The variables the store's live tests read, named here so a run with none of them says which are
# missing rather than counting the skips they cause.
missing=()
for name in "${variables[@]}"; do
  [ -n "${!name:-}" ] || missing+=("$name")
done
if [ ${#missing[@]} -gt 0 ]; then
  echo "store-live-check: the $store live tests need ${missing[*]}, which the environment does not set" >&2
  exit 1
fi

# The stream go test writes is kept, so each case of the run can be read afterwards: at
# STORE_LIVE_CHECK_EVENTS when set, else in a file of its own, and its path is printed.
events="${STORE_LIVE_CHECK_EVENTS:-$(mktemp -t store-live-check-events.XXXXXX)}"

# -run Live selects the live tests of the package; every other test of it is left out, so a
# failure here is a failure against the real service. go test's own exit code is kept and read
# below with the counts, since a build failure writes no action events either.
set +e
go test -json -count=1 -tags "$tag" -run Live "${packages[@]}" >"$events" 2>&1
go_exit=$?
set -e

counts="$(
  awk -F'"' '
    /"Action":"pass"/ && /"Test":"/ { pass++ }
    /"Action":"skip"/ && /"Test":"/ { skip++ }
    /"Action":"fail"/ && /"Test":"/ { fail++ }
    END { printf "%d %d %d", pass, skip, fail }
  ' "$events"
)"
read -r passed skipped failed <<<"$counts"

# tests_with is the name of each test whose action is $1 (pass, skip or fail), one per line, and
# nothing for an action no test took.
tests_with() {
  grep -o "\"Action\":\"$1\",\"Package\":\"[^\"]*\",\"Test\":\"[^\"]*\"" "$events" |
    sed -E 's/.*"Test":"([^"]*)"/\1/' || true
}

if [ "$((passed + skipped + failed))" -eq 0 ]; then
  echo "UNKNOWN: go test wrote no events, so nothing ran and this run says nothing about the $store store (go test exited $go_exit)" >&2
  sed -n '1,40p' "$events" >&2
  exit 1
fi

echo "passed=$passed skipped=$skipped failed=$failed"
# Each skipped or failed test by name; grep finds none in a run with neither, which is no error.
for action in skip:skipped fail:failed; do
  tests_with "${action%%:*}" | sed -E "s/^/${action##*:}: /" | sort -u
done

# Each of the store's live contracts itself: its test, its store's level and its cases, each
# passed. A store level passes only when every case of it passed, so with no skip and no failure
# this is that contract run against the real service; a run of other live tests alone, or of one
# contract alone, a contract renamed or deleted, is not.
missing_contracts=()
for contract in "${contracts[@]}"; do
  contract_passed="$(tests_with pass | grep -cxF "$contract" || true)"
  store_passed="$(tests_with pass | grep -cxF "$contract/$store" || true)"
  cases_passed="$(tests_with pass | grep -cF "$contract/$store/" || true)"
  echo "live contract $contract: $cases_passed cases passed"
  if [ "$contract_passed" -eq 0 ] || [ "$store_passed" -eq 0 ] || [ "$cases_passed" -eq 0 ]; then
    missing_contracts+=("$contract")
  fi
done
echo "events: $events"

if [ "$passed" -eq 0 ] || [ "$skipped" -ne 0 ] || [ "$failed" -ne 0 ] || [ "$go_exit" -ne 0 ]; then
  echo "store-live-check: the $store live run is not a pass (go test exited $go_exit)" >&2
  exit 1
fi
if [ ${#missing_contracts[@]} -gt 0 ]; then
  echo "store-live-check: the $store live run holds no passing ${missing_contracts[*]} with its cases, so it is no live contract run" >&2
  exit 1
fi
