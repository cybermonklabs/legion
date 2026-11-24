#!/usr/bin/env bash
# packages/envoy/scripts/store-live-check.test.sh
#
# Drives store-live-check.sh over recorded `go test -json` streams through a stub `go` on PATH, so
# it runs no live test and reaches no cloud: a run whose tests all passed is a pass, one with a
# skip or a failure is not, and a run that wrote no event says UNKNOWN. Also checks that it
# refuses a run whose store variables are unset before it runs anything.
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
  out="$(PATH="$WORK/bin:$PATH" BROKER_GCP_LIVE_PROJECT=example BROKER_GCP_LIVE_KMS_KEY=key "$CHECK" gcp 2>&1)"
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
{"Action":"pass","Package":"p","Test":"TestGCPLiveRunsTheStoreContract"}
{"Action":"pass","Package":"p","Elapsed":4.2}'

stub_go "$passed_events" 0
run_check
check_row "a run whose live tests all passed" 0 "passed=2 skipped=0 failed=0"

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
