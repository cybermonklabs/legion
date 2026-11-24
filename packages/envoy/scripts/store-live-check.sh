#!/usr/bin/env bash
# packages/envoy/scripts/store-live-check.sh
#
# Runs one secret store's live contract tests against that store's real service and counts what
# they did: `store-live-check.sh <store>` with <store> the store's registry id. It prints
# `passed=N skipped=M failed=K` and the name of each skipped or failed test, and exits 0 only
# when N is above zero and M and K are both zero.
#
# A skip counts as a failure because a live test skips when its variables are unset, which is the
# shape a live run fails in: the tests pass, nothing ran, and the run looks green. A run that
# wrote no event at all prints UNKNOWN and exits 1 rather than reading an empty set as a pass.
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

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
events="$work/events.json"

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

if [ "$((passed + skipped + failed))" -eq 0 ]; then
  echo "UNKNOWN: go test wrote no events, so nothing ran and this run says nothing about the $store store (go test exited $go_exit)" >&2
  sed -n '1,40p' "$events" >&2
  exit 1
fi

echo "passed=$passed skipped=$skipped failed=$failed"
# Each skipped or failed test by name; grep finds none in a run with neither, which is no error.
for action in skip:skipped fail:failed; do
  grep -o "\"Action\":\"${action%%:*}\",\"Package\":\"[^\"]*\",\"Test\":\"[^\"]*\"" "$events" |
    sed -E "s/.*\"Test\":\"([^\"]*)\"/${action##*:}: \\1/" | sort -u || true
done

if [ "$passed" -eq 0 ] || [ "$skipped" -ne 0 ] || [ "$failed" -ne 0 ] || [ "$go_exit" -ne 0 ]; then
  echo "store-live-check: the $store live run is not a pass (go test exited $go_exit)" >&2
  exit 1
fi
