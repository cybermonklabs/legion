#!/usr/bin/env bash
# packages/envoy/scripts/database-skips.sh
#
# Counts the tests of the broker's packages (internal/broker/...) that skipped, from a `go test
# -json` report, and fails on any. The verification's check c8 says the
# database broker suites run with BROKER_TEST_DATABASE_URL set and none skips; each of them skips,
# which go test counts as a pass, when that variable is unset, and a t.Skip left in one skips it
# with the variable set. With no argument it runs, from packages/envoy, one
#
#   go test -json -count=1 -timeout "${GO_TEST_TIMEOUT:-15m}" <packages>
#
# over the packages `go list ./internal/broker/... | grep -Ev '/internal/broker/natscallout$'`
# prints, and reads that report. The grep leaves out the broker's packages the race lane runs
# (each a package of ENVOY_RACE_LANE in .github/scripts/ci_job_reads.py, which derives the same
# pattern as DATABASE_SKIPS_PATTERN and refuses this script when its grep says otherwise): the auth
# service's natscallout opens no database, and envoy-go-race runs it under the detector, so a run
# here would be its second in the same CI run. The -timeout bounds each package's test binary, not
# the run, since the packages run several at a time, so a caller that must bound the run sets a
# limit of its own (CI's step does). `--report FILE` reads FILE instead and runs nothing, which a
# control uses. It prints each skipped test as `SKIP PACKAGE TEST`, then `database_skips=N`, and
# for a run of its own `go_test_exit=E`, go test's own exit status. A report that is missing, empty
# or holds no test of a broker package is no count at all: it prints `database_skips=UNKNOWN` and
# names what it could not read, rather than a zero. It exits 0 only when the report shows broker
# tests run, none of them skipped, and, for a run of its own, go test passed; 1 otherwise.
#
# A skip whose test is named in EXPECTED_SKIPS below is no database suite's and is not counted:
# each one skips by its own design, whatever the database, and the list says why.
set -euo pipefail

cd "$(dirname "$0")/.."
broker='github.com/sjawhar/envoy/internal/broker'

# EXPECTED_SKIPS: PACKAGE TEST, one per line, each followed by the reason it skips in every run.
# FuzzAirlock, TestModelAgreesOnHostShapes and TestTheModelReadsAHostlessEntryWithGuidance need
# AIRLOCK_MODEL, a built Lean model of the airlock; the airlock-lean job builds one, and its
# "Compare the Go airlock with the model over the seed corpus" step runs all three.
EXPECTED_SKIPS='github.com/sjawhar/envoy/internal/broker/airlock FuzzAirlock needs AIRLOCK_MODEL
github.com/sjawhar/envoy/internal/broker/airlock TestModelAgreesOnHostShapes needs AIRLOCK_MODEL
github.com/sjawhar/envoy/internal/broker/airlock TestTheModelReadsAHostlessEntryWithGuidance needs AIRLOCK_MODEL'

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

report=""
go_test_exit=""
case "${1:-}" in
  --report)
    report="${2:-}"
    if [ -z "$report" ]; then
      echo "database-skips.sh: --report needs the go test -json report to read" >&2
      exit 1
    fi
    ;;
  "")
    report="$work/report.json"
    go_test_exit=0
    listed="$(go list ./internal/broker/... | grep -Ev '/internal/broker/natscallout$')" || {
      echo "database-skips.sh: go list of the broker's packages failed, so no test ran" >&2
      exit 1
    }
    mapfile -t packages <<<"$listed"
    go test -json -count=1 -timeout "${GO_TEST_TIMEOUT:-15m}" "${packages[@]}" >"$report" 2>"$work/stderr" || go_test_exit=$?
    ;;
  *)
    echo "database-skips.sh: unknown argument $1 (run it with none, or with --report FILE)" >&2
    exit 1
    ;;
esac

unknown() {
  echo "database_skips=UNKNOWN"
  echo "database-skips.sh: $1, so no skip was counted" >&2
  [ -z "$go_test_exit" ] || { echo "go_test_exit=$go_test_exit"; cat "$work/stderr" >&2; }
  exit 1
}

[ -e "$report" ] || unknown "the report $report does not exist"
[ -s "$report" ] || unknown "the report $report is empty"
ran="$(jq -r --arg broker "$broker" 'select(.Test != null and (.Action == "pass" or .Action == "fail" or .Action == "skip") and ((.Package // "") | startswith($broker))) | .Test' "$report" 2>"$work/jq" | wc -l)" ||
  unknown "the report $report is not a go test -json stream ($(head -c 200 "$work/jq"))"
[ "$ran" -gt 0 ] || unknown "the report $report holds no test of a package under ${broker#github.com/sjawhar/envoy/}"

jq -r --arg broker "$broker" 'select(.Action == "skip" and .Test != null and ((.Package // "") | startswith($broker))) | "\(.Package) \(.Test)"' "$report" | sort -u >"$work/skipped"
printf '%s\n' "$EXPECTED_SKIPS" | sed '/^$/d' | cut -d' ' -f1,2 | sort -u >"$work/expected"
comm -23 "$work/skipped" "$work/expected" >"$work/counted"
sed 's/^/SKIP /' "$work/counted"
database_skips="$(wc -l <"$work/counted")"
echo "database_skips=$database_skips"

status=0
[ "$database_skips" -eq 0 ] || status=1
if [ -n "$go_test_exit" ]; then
  echo "go_test_exit=$go_test_exit"
  if [ "$go_test_exit" -ne 0 ]; then
    status=1
    jq -r 'select(.Action == "fail" and .Test != null) | "FAIL \(.Package) \(.Test)"' "$report" >&2
    jq -rsj '[.[] | select(.Action == "fail" and .Test != null) | [.Package, .Test]] as $failed | .[] | select(.Action == "output" and .Test != null) | select([.Package, .Test] as $t | any($failed[]; . == $t)) | .Output' "$report" >&2
    cat "$work/stderr" >&2
  fi
fi
exit "$status"
