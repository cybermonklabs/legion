#!/usr/bin/env bash
# packages/envoy/scripts/store-live-check.test.sh
#
# Drives store-live-check.sh over recorded `go test -json` streams through a stub `go` on PATH, so
# it runs no live test and reaches no cloud: a run whose tests all passed, the store's live
# contract and its cases among them, is a pass, and keeps its events; one with a skip or a failure
# is not, nor one whose passing tests hold no live contract or no case of it, and a run that wrote
# no event says UNKNOWN. Also checks that it refuses a run whose store variables are unset before
# it runs anything.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/store-live-check.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
failures=0

# stub_go writes a `go` on PATH that prints the recorded stream in $1 and exits $2, so the check
# reads what a real run of those tests would have written.
stub_go() {
  local events="$1" exit_code="$2"
  mkdir -p "$WORK/bin"
  cat >"$WORK/bin/go" <<EOF
#!/usr/bin/env bash
cat <<'EVENTS'
$events
EVENTS
exit $exit_code
EOF
  chmod +x "$WORK/bin/go"
}

# run_check runs the check with the stub on PATH and the live variables set, and answers its
# output and exit code in $out and $code.
run_check() {
  set +e
  out="$(PATH="$WORK/bin:$PATH" STORE_LIVE_CHECK_EVENTS="$WORK/events.json" BROKER_GCP_LIVE_PROJECT=example BROKER_GCP_LIVE_KMS_KEY=key "$CHECK" gcp 2>&1)"
  code=$?
  set -e
}

# check_row runs one row and reports it.
check_row() {
  local label="$1" want_code="$2" want_text="$3"
  if [ "$code" -ne "$want_code" ]; then
    echo "FAIL: $label exits $code, want $want_code: $out" >&2
    failures=$((failures + 1))
    return
  fi
  if [ -n "$want_text" ] && ! grep -qF -- "$want_text" <<<"$out"; then
    echo "FAIL: $label does not say '$want_text': $out" >&2
    failures=$((failures + 1))
    return
  fi
  printf 'PASS: %s\n' "$label"
}

passed_events='{"Action":"run","Package":"p","Test":"TestGCPLiveRunsTheStoreContract"}
{"Action":"pass","Package":"p","Test":"TestGCPLiveRunsTheStoreContract/gcp/Describe_answers_what_List_answers_for_one_name"}
{"Action":"pass","Package":"p","Test":"TestGCPLiveRunsTheStoreContract/gcp/a_new_value_moves_Version"}
{"Action":"pass","Package":"p","Test":"TestGCPLiveRunsTheStoreContract/gcp"}
{"Action":"pass","Package":"p","Test":"TestGCPLiveRunsTheStoreContract"}
{"Action":"pass","Package":"p","Elapsed":4.2}'

stub_go "$passed_events" 0
run_check
check_row "a run whose live tests all passed" 0 "passed=4 skipped=0 failed=0"
check_row "a passing run names its contract's cases" 0 "live contract TestGCPLiveRunsTheStoreContract: 2 cases passed"
check_row "a passing run names where its events are kept" 0 "events: $WORK/events.json"
if [ "$(cat "$WORK/events.json")" = "$passed_events" ]; then
  printf 'PASS: %s\n' "the run's events are kept"
else
  echo "FAIL: $WORK/events.json does not hold the run's events" >&2
  failures=$((failures + 1))
fi

# A run whose only passing test is another live test, with no event of the live contract: the
# contract deleted or renamed, which would otherwise count as a pass.
legacy_events='{"Action":"run","Package":"p","Test":"TestGCPLive"}
{"Action":"pass","Package":"p","Test":"TestGCPLive"}
{"Action":"pass","Package":"p","Elapsed":1.0}'

stub_go "$legacy_events" 0
run_check
check_row "a run of another live test alone" 1 "holds no passing TestGCPLiveRunsTheStoreContract with its cases"

# The contract's test and its store's level pass with no case under them: a contract whose
# cases all went, which runs nothing against the service.
no_case_events='{"Action":"pass","Package":"p","Test":"TestGCPLiveRunsTheStoreContract/gcp"}
{"Action":"pass","Package":"p","Test":"TestGCPLiveRunsTheStoreContract"}
{"Action":"pass","Package":"p","Elapsed":0.1}'

stub_go "$no_case_events" 0
run_check
check_row "a run of the contract with no case" 1 "live contract TestGCPLiveRunsTheStoreContract: 0 cases passed"

# With STORE_LIVE_CHECK_EVENTS unset, the events go to a file of their own, which is kept.
stub_go "$passed_events" 0
set +e
out="$(PATH="$WORK/bin:$PATH" TMPDIR="$WORK" env -u STORE_LIVE_CHECK_EVENTS BROKER_GCP_LIVE_PROJECT=example BROKER_GCP_LIVE_KMS_KEY=key "$CHECK" gcp 2>&1)"
code=$?
set -e
kept="$(sed -n 's/^events: //p' <<<"$out")"
check_row "a run with no events path set" 0 "events: $WORK/store-live-check-events."
if [ -n "$kept" ] && [ "$(cat "$kept")" = "$passed_events" ]; then
  printf 'PASS: %s\n' "a run with no events path set keeps its events"
else
  echo "FAIL: a run with no events path set kept no events at '$kept': $out" >&2
  failures=$((failures + 1))
fi

skipped_events='{"Action":"pass","Package":"p","Test":"TestGCPLiveRunsTheStoreContract/gcp/a_new_value_moves_Version"}
{"Action":"skip","Package":"p","Test":"TestGCPLiveReadsASecretItWrote"}
{"Action":"pass","Package":"p","Elapsed":1.1}'

stub_go "$skipped_events" 0
run_check
check_row "a run with a skip" 1 "skipped: TestGCPLiveReadsASecretItWrote"

failed_events='{"Action":"pass","Package":"p","Test":"TestGCPLiveRunsTheStoreContract/gcp/Describe_answers_what_List_answers_for_one_name"}
{"Action":"fail","Package":"p","Test":"TestGCPLiveRunsTheStoreContract/gcp/a_new_value_moves_Version"}
{"Action":"fail","Package":"p","Elapsed":3.3}'

stub_go "$failed_events" 1
run_check
check_row "a run with a failed case" 1 "failed: TestGCPLiveRunsTheStoreContract/gcp/a_new_value_moves_Version"

stub_go "" 1
run_check
check_row "a run that wrote no event" 1 "UNKNOWN: go test wrote no events"

set +e
out="$(PATH="$WORK/bin:$PATH" env -u BROKER_GCP_LIVE_PROJECT -u BROKER_GCP_LIVE_KMS_KEY "$CHECK" gcp 2>&1)"
code=$?
set -e
check_row "a run with the store's variables unset" 1 "BROKER_GCP_LIVE_PROJECT BROKER_GCP_LIVE_KMS_KEY"

set +e
out="$(PATH="$WORK/bin:$PATH" "$CHECK" 2>&1)"
code=$?
set -e
check_row "a run naming no store" 2 "usage: store-live-check.sh <store>"

set +e
out="$(PATH="$WORK/bin:$PATH" "$CHECK" vault 2>&1)"
code=$?
set -e
check_row "a run naming a store with no live tests" 2 "no live tests for the store vault"

if [ "$failures" -ne 0 ]; then
  echo "$failures row(s) failed" >&2
  exit 1
fi
echo "store-live-check.test.sh: every row passed"
