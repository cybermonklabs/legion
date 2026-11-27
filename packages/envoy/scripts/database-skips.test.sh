#!/usr/bin/env bash
# packages/envoy/scripts/database-skips.test.sh
#
# Tests database-skips.sh, the skip counter of the envoy module's database-backed packages, with
# eight controls. Each control must fail as it states, or this test fails; each beside it a run
# the counter must pass, so a counter that fails everything fails here too:
#
#   1. a skipped test planted in ./internal/cipher/api/, ./cmd/cipherd/, ./internal/holdrecord/
#      and ./internal/dispatch/api/, one at a time, through a Go overlay (GOFLAGS=-overlay) that adds
#      zz_planted_skip_test.go, is printed as `SKIP <package> TestPlantedSkip` and fails the
#      counter; the same run without the overlay passes;
#   2. with its variable unset or empty, the counter prints `database_skips=UNKNOWN` naming it,
#      runs no test and fails;
#   3. --report of a report that shows no run prints `database_skips=UNKNOWN` and fails: one
#      holding no test of a package the patterns name, one whose only tests there are expected
#      skips, one where a named package with test files passed with no test or is missing, a
#      missing and an empty report, and an EXPECTED_SKIPS line whose test the report does not
#      hold; a report holding a failed test fails, and one holding a data race report filed under
#      a subtest that passed prints that report and no passed test's own line; a test the binary
#      runs twice under one name (a TestMain that calls m.Run twice) that fails in one run and
#      passes in the other has its failure's lines printed, whichever run failed, and none of the
#      passing run's own, and one that passed in the first run and was running when the second
#      timed out is printed as unfinished; a package spelled other than canonically
#      (`./cmd/./cipherd/`, `.//cmd/cipherd/`, `./cmd/cipherd/../cipherd/`) is still counted; and a
#      report of the broker's tests whose only skips are the expected ones passes;
#   4. --timeout 1s on ./internal/cipher/api/, beside a planted parallel test that runs three
#      seconds and one that passes first, ends the package's binary on its timeout: the counter
#      prints a go_test_exit that is not 0, the timeout's panic (which test2json files under the
#      passed test) and the unfinished test, and fails; without --timeout the same run passes;
#   5. a copy of the counter with TestSearchLatencyOnCorpus's EXPECTED_SKIPS line deleted, in a
#      temporary checkout of this commit (git worktree add; Go's -overlay replaces Go files alone,
#      so it cannot reach the script's table), run on ./internal/dispatch/api/, prints that test's
#      SKIP line and fails, where the counter itself passes the same run; a copy whose table line
#      gives no reason is refused; and with either of `--var DISPATCH_TEST_DATABASE_URL --var
#      BROKER_TEST_DATABASE_URL` unset or empty beside the other set, the counter prints
#      `database_skips=UNKNOWN` naming that one and runs no test.
#   6. --count-table over internal/fold, which is no package of the table, beside the broker
#      API's package, which is: a skip planted in internal/fold is not counted and the run passes,
#      printing both packages' times; a skip planted in the API's package is counted and fails the
#      counter; a failure planted in internal/fold fails the counter, which prints it; a run of
#      internal/fold alone prints database_skips=NONE and passes, and fails on that planted
#      failure; and the option is refused beside --report and with no package.
#   7. -run, as a leg of envoy-go-race-api pipes it in: a run of the API package under a -run that
#      leaves most of its EXPECTED_SKIPS tests out passes, those lines being other legs'; a copy of
#      the counter with a line for a test the package does not have reads UNKNOWN under a -run and
#      names that line alone; a name with a non-ASCII letter is taken; a -run that is no list of
#      whole test names is refused before any test runs; and the option is refused beside --report.
#   8. a control that passes prints no line that a reader or a grep of the CI log takes for a
#      test failure (MARKER): judge fails, and counts as failed, a passing control whose evidence
#      holds one, each kind of marker wherever a run's printed lines put it, and fails none for a
#      skip, a count or a package's time; it prints the output of a control's planted tests only
#      when that control fails, and on neither stream when it passes; a grep for failed tests of a
#      log holding a real failure and judge's lines finds the real test by its own name, and the
#      planted one only as TestPlanted..., on the line of the control that failed; and every
#      failed or unfinished test that any run of controls 1 to 7 printed is named TestPlanted...,
#      and they printed some, read by a scan that finds a real test's failure in each form go test
#      and the counter print one, as text, and fails on a file it cannot read. A grep of CI logs for
#      `--- FAIL: ` found TestTwice, then a planted test of control 3, in 14 of the 60 failed
#      merge-group runs of 2026-11-25 and 26, and took it for the most frequent failing test,
#      though it never failed: this script printed its planted failure in every run, beside the
#      PASS: line of the control that planted it.
#
# Every run narrows its packages to the tests it is about with
# GOFLAGS=-run=..., which go test reads as its own -run (and go list ignores): what the counter
# makes of a report does not depend on how many other tests passed in it, and the API package's
# whole run is the race job's. The ./internal/dispatch/api/ runs name every test EXPECTED_SKIPS
# lists for the package, which the counter holds a run of that package to, and one test that
# passes, since a package whose tests all skip shows no run.
#
# Every test this script plants, whether in a package through an overlay or as a --report
# report's events, is named TestPlanted...; a report's other events name real tests of the
# package, which it needs to show a run. A planted failure that a failed control prints so says it
# was planted.
#
# It prints a PASS: or FAIL: line per property and then `database-skips.test.sh: passed` or
# `database-skips.test.sh: failed (<n> of <m>)`, and exits 0 or 1.
#
# Usage: packages/envoy/scripts/database-skips.test.sh
#
# Needs go, jq and git, this repository's git checkout, and a scratch Postgres named by both
# BROKER_TEST_DATABASE_URL and DISPATCH_TEST_DATABASE_URL, as ci.yaml's envoy-go job sets them;
# with either unset it fails, naming it, since every control but 2, 3 and 8 runs database tests.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY="$(cd "$SCRIPT_DIR/.." && pwd)"
COUNTER="$SCRIPT_DIR/database-skips.sh"
MODULE="$(awk '$1 == "module" { print $2; exit }' "$ENVOY/go.mod")"
API="$MODULE/internal/dispatch/api"
# The harness's own go list calls print go's messages, a module proxy's reply among them, inside a
# ci-log-guard.sh block, where they cannot become workflow commands.
# shellcheck source=.github/scripts/ci-log-guard.sh
. "$(cd "$SCRIPT_DIR/../../.." && pwd)/.github/scripts/ci-log-guard.sh"
# A test of the API package that passes in any run, which its narrowed runs hold beside the
# skips.
API_ANCHOR=TestTheDeploymentsRelatedCheckAllowanceIsTheDocumentedBound

WORK="$(mktemp -d)"
TREE=""
cleanup() {
  if [ -n "$TREE" ]; then
    git -C "$ENVOY" worktree remove --force "$TREE" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

PASSED=0
FAILED=0
# MARKER: what a reader or a grep of a CI log takes for a test failure, in evidence printed on one
# line: go test's `--- FAIL: `, a panic, the race detector's report, and a printed line that is
# FAIL or starts `FAIL ` (go test's package line, and the counter's `FAIL <package> <test>`) or
# `UNFINISHED ` (the counter's). A printed line starts the evidence, follows a | (seen joins a
# run's lines with one), or follows `printed ` or `stderr `, which name what a run printed.
MARKER='--- FAIL: |panic: |WARNING: DATA RACE|(^|[|]|printed |stderr )(FAIL|UNFINISHED)([[:space:]|]|$)'
# judge <0 for a pass> <property> <what was seen> [<what the control's planted tests printed>]:
# the fourth argument is printed only when the property fails, and a property that passes fails
# when what was seen holds a MARKER (control 8).
judge() {
  if [ "$1" != 0 ]; then
    FAILED=$((FAILED + 1))
    printf 'FAIL: %s — %s%s\n' "$2" "$3" "${4:+; its planted tests printed $4}"
  elif grep -qE -e "$MARKER" <<<"$3"; then
    FAILED=$((FAILED + 1))
    printf 'FAIL: %s — %s: %s\n' "$2" \
      "it passed, but what it saw holds a line a reader of the log takes for a test failure (MARKER), which only judge's fourth argument may hold" "$3"
  else
    PASSED=$((PASSED + 1))
    printf 'PASS: %s — %s\n' "$2" "$3"
  fi
}

for name in BROKER_TEST_DATABASE_URL DISPATCH_TEST_DATABASE_URL; do
  if [ -z "${!name:-}" ]; then
    echo "database-skips.test.sh: $name is unset or empty, so the controls that run database tests cannot run" >&2
    echo "database-skips.test.sh: failed (UNKNOWN: $name)"
    exit 1
  fi
done

# count NAME COMMAND...: runs COMMAND, keeping its stdout, stderr and exit status as NAME. It also
# adds both to runs.log, which keeps every run's output for control 8, though a later run of the
# same NAME replaces NAME's own files; each ends with a line break there, so that a last line
# without one does not run into the next file's first.
count() {
  local name="$1"
  shift
  local code=0
  "$@" >"$WORK/$name.out" 2>"$WORK/$name.err" || code=$?
  echo "$code" >"$WORK/$name.exit"
  { cat "$WORK/$name.out"; echo; cat "$WORK/$name.err"; echo; } >>"$WORK/runs.log"
}
out() { cat "$WORK/$1.out"; }
code() { cat "$WORK/$1.exit"; }
# seen NAME: the run's exit status and what it printed, on one line.
seen() { printf 'exit %s, printed %s' "$(code "$1")" "$(tr '\n' '|' <"$WORK/$1.out")"; }
# said NAME TEXT: whether the run's stderr holds TEXT.
said() { grep -qF -- "$2" "$WORK/$1.err"; }

# table_tests PACKAGE: the tests EXPECTED_SKIPS lists for PACKAGE, read off the counter's own
# table, one per line.
table_tests() { sed -n "s|^\(EXPECTED_SKIPS='\)\{0,1\}$1 \([A-Za-z0-9_]*\) .*|\2|p" "$COUNTER"; }
airlock="$MODULE/internal/cipher/proxyrules"
api_expected="$(table_tests "$API" | paste -sd '|')"
airlock_expected="$(table_tests "$airlock" | paste -sd '|')"
if [ -z "$api_expected" ] || [ -z "$airlock_expected" ]; then
  echo "database-skips.test.sh: EXPECTED_SKIPS in $COUNTER names no test of $API or of $airlock, which controls 3 and 5 read" >&2
  echo "database-skips.test.sh: failed (UNKNOWN: the expected-skip table)"
  exit 1
fi

# plant NAME DIR BODY: an overlay file adding DIR/zz_planted_skip_test.go, in DIR's package, holding
# BODY; its path is NAME.overlay.
plant() {
  local name="$1" dir="$2" body="$3" package
  package="$(cd "$ENVOY" && ci_log_guard_stderr go list -f '{{.Name}}' "./$dir/")"
  [ ! -e "$ENVOY/$dir/zz_planted_skip_test.go" ] || {
    echo "database-skips.test.sh: $dir/zz_planted_skip_test.go exists, so an overlay would replace a real file" >&2
    exit 1
  }
  printf 'package %s\n\nimport (\n\t"testing"\n\t"time"\n)\n\nvar _ = time.Second\n\n%s\n' "$package" "$body" >"$WORK/$name.go"
  jq -n --arg path "$ENVOY/$dir/zz_planted_skip_test.go" --arg file "$WORK/$name.go" '{Replace: {($path): $file}}' >"$WORK/$name.overlay"
}
SKIP_BODY='func TestPlantedSkip(t *testing.T) { t.Skip("planted") }'
# Two parallel tests: the quick one's `--- PASS` comes before the slow one's timeout, and
# test2json files the panic after it under the quick test, which the counter must still print.
WAIT_BODY='func TestPlantedWait(t *testing.T) { t.Parallel(); time.Sleep(3 * time.Second) }

func TestPlantedQuick(t *testing.T) { t.Parallel(); time.Sleep(100 * time.Millisecond) }'

echo "database-skips.test.sh: control 1, a planted skip in each of four packages" >&2
# Each package's -run names the planted test and a test the package passes with the database set
# (the API package's, the tests its table excuses), so the run without the overlay holds tests.
for row in "internal/cipher/api ^(TestPlantedSkip|TestHealthzIsPublic)$" \
  "cmd/cipherd ^(TestPlantedSkip|TestBootBindsOnlyAfterTheCatchUp)$" \
  "internal/holdrecord ^(TestPlantedSkip|TestNewRefusesAnIncompleteConfiguration)$" \
  "internal/dispatch/api ^(TestPlantedSkip|$API_ANCHOR|$api_expected)$"; do
  read -r dir run <<<"$row"
  name="planted-${dir//\//-}"
  plant "$name" "$dir" "$SKIP_BODY"
  count "$name" env GOFLAGS="-overlay=$WORK/$name.overlay -run=$run" bash "$COUNTER" \
    --var BROKER_TEST_DATABASE_URL --var DISPATCH_TEST_DATABASE_URL "./$dir/"
  judge "$([ "$(code "$name")" = 1 ] && [ "$(grep -c '^SKIP ' "$WORK/$name.out")" = 1 ] &&
    grep -qxF "SKIP $MODULE/$dir TestPlantedSkip" "$WORK/$name.out" &&
    grep -qxF 'database_skips=1' "$WORK/$name.out" && echo 0 || echo 1)" \
    "control 1: a test planted in ./$dir/ that skips is counted, and fails the counter" "$(seen "$name")"
  count "$name-none" env GOFLAGS="-run=$run" bash "$COUNTER" \
    --var BROKER_TEST_DATABASE_URL --var DISPATCH_TEST_DATABASE_URL "./$dir/"
  judge "$([ "$(code "$name-none")" = 0 ] && grep -qxF 'database_skips=0' "$WORK/$name-none.out" &&
    grep -qxF 'go_test_exit=0' "$WORK/$name-none.out" && echo 0 || echo 1)" \
    "control 1: the same run of ./$dir/ without the planted test passes" "$(seen "$name-none")"
done

echo "database-skips.test.sh: control 2, the variable unset" >&2
# Narrowed like every run here, so a counter that ran with the variable unset would take seconds,
# not the default packages' whole run, and fail this control rather than the step's time limit.
# The first run calls the counter by its path, as both CI steps do, so a counter that lost its
# executable bit fails here (exit 126, nothing printed) rather than in the merge queue.
default_run="-run=^(TestHealthzIsPublic|TestBootBindsOnlyAfterTheCatchUp|$airlock_expected)$"
count unset env -u BROKER_TEST_DATABASE_URL GOFLAGS="$default_run" "$COUNTER"
judge "$([ "$(code unset)" = 1 ] && [ "$(out unset)" = 'database_skips=UNKNOWN' ] &&
  said unset 'BROKER_TEST_DATABASE_URL is unset or empty' && echo 0 || echo 1)" \
  "control 2: with BROKER_TEST_DATABASE_URL unset the counter prints UNKNOWN naming it and runs no test" "$(seen unset)"
count empty env BROKER_TEST_DATABASE_URL= GOFLAGS="$default_run" bash "$COUNTER"
judge "$([ "$(code empty)" = 1 ] && [ "$(out empty)" = 'database_skips=UNKNOWN' ] &&
  said empty 'BROKER_TEST_DATABASE_URL is unset or empty' && echo 0 || echo 1)" \
  "control 2: with BROKER_TEST_DATABASE_URL empty the counter prints UNKNOWN naming it and runs no test" "$(seen empty)"

echo "database-skips.test.sh: control 3, reports" >&2
# airlock_skips: a skip event of each test EXPECTED_SKIPS lists for the proxyrules package.
airlock_skips() { table_tests "$airlock" | while IFS= read -r test; do ev skip "$airlock" "$test"; done; }
brokerapi="$MODULE/internal/cipher/api"
cmdbroker="$MODULE/cmd/cipherd"
# ev ACTION PACKAGE [TEST]: one event of a go test -json report.
ev() {
  if [ $# -eq 3 ]; then
    jq -nc --arg a "$1" --arg p "$2" --arg t "$3" '{Action: $a, Package: $p, Test: $t}'
  else
    jq -nc --arg a "$1" --arg p "$2" '{Action: $a, Package: $p}'
  fi
}
# unknown_report NAME TEXT PROPERTY: the run was UNKNOWN, exit 1, and said TEXT.
unknown_report() {
  judge "$([ "$(code "$1")" = 1 ] && [ "$(out "$1")" = 'database_skips=UNKNOWN' ] && said "$1" "$2" && echo 0 || echo 1)" \
    "control 3: $3" "$(seen "$1")"
}
{ ev pass "$MODULE/internal/dispatch/store" TestPlantedElsewhere; ev skip "$MODULE/internal/dispatch/store" TestPlantedSkipElsewhere
  ev pass "$MODULE/internal/dispatch/store"; } >"$WORK/elsewhere.json"
count elsewhere bash "$COUNTER" --report "$WORK/elsewhere.json"
unknown_report elsewhere 'holds no test of a package under ./internal/cipher/... ./cmd/cipherd/ ./internal/holdrecord/ that passed or failed' \
  "a report holding no test of a package the patterns name is UNKNOWN"
: >"$WORK/empty.json"
count empty-report bash "$COUNTER" --report "$WORK/empty.json"
count missing-report bash "$COUNTER" --report "$WORK/missing.json"
judge "$([ "$(code empty-report)" = 1 ] && [ "$(out empty-report)" = 'database_skips=UNKNOWN' ] &&
  [ "$(code missing-report)" = 1 ] && [ "$(out missing-report)" = 'database_skips=UNKNOWN' ] && echo 0 || echo 1)" \
  "control 3: an empty and a missing report are UNKNOWN" "empty: $(seen empty-report); missing: $(seen missing-report)"
{ airlock_skips; ev pass "$airlock"; } >"$WORK/all-expected.json"
count all-expected bash "$COUNTER" --report "$WORK/all-expected.json" ./internal/cipher/proxyrules/
unknown_report all-expected 'holds no test of a package under ./internal/cipher/proxyrules/ that passed or failed' \
  "a report whose only tests are expected skips is UNKNOWN"
{ ev pass "$brokerapi" TestHealthzIsPublic; ev pass "$brokerapi"; ev pass "$cmdbroker"; } >"$WORK/silent.json"
count silent bash "$COUNTER" --report "$WORK/silent.json" ./internal/cipher/api/ ./cmd/cipherd/
unknown_report silent "holds no test that passed or failed in $cmdbroker, which has test files" \
  "a package that passed with no test, beside one whose tests ran, is UNKNOWN"
{ ev pass "$brokerapi" TestHealthzIsPublic; ev pass "$brokerapi"; } >"$WORK/partial.json"
count partial bash "$COUNTER" --report "$WORK/partial.json" ./internal/cipher/api/ ./cmd/cipherd/
unknown_report partial "holds no test that passed or failed in $cmdbroker, which has test files" \
  "a named package the report does not hold, beside one whose tests ran, is UNKNOWN"
{ ev pass "$brokerapi" TestHealthzIsPublic; ev fail "$brokerapi" TestPlantedFailure; ev fail "$brokerapi"; } >"$WORK/failed.json"
count failed bash "$COUNTER" --report "$WORK/failed.json" ./internal/cipher/api/
judge "$([ "$(code failed)" = 1 ] && [ "$(out failed)" = 'database_skips=0' ] && said failed "FAIL $brokerapi TestPlantedFailure" && echo 0 || echo 1)" \
  "control 3: a report holding a failed test fails, naming it" "$(seen failed)" "$(tr '\n' '|' <"$WORK/failed.err")"
# A data race report the detector prints after a subtest's `--- PASS` is filed under that passed
# subtest, as test2json files it; the counter must print it, and no line a passed test printed
# before its own `--- PASS`.
oev() { jq -nc --arg p "$brokerapi" --arg t "$1" --arg o "$2" '{Action: "output", Package: $p, Test: $t, Output: $o}'; }
{ oev TestPlantedPassBeside 'a line TestPlantedPassBeside printed itself'$'\n'; oev TestPlantedPassBeside '--- PASS: TestPlantedPassBeside (0.00s)'$'\n'
  ev pass "$brokerapi" TestPlantedPassBeside
  oev TestPlantedRace/sub '--- PASS: TestPlantedRace/sub (0.00s)'$'\n'; ev pass "$brokerapi" TestPlantedRace/sub
  oev TestPlantedRace/sub 'WARNING: DATA RACE'$'\n'; oev TestPlantedRace/sub 'Write at 0x00c000 by goroutine 7:'$'\n'
  oev TestPlantedRace '    testing.go:1617: race detected during execution of test'$'\n'; oev TestPlantedRace '--- FAIL: TestPlantedRace (0.01s)'$'\n'
  ev fail "$brokerapi" TestPlantedRace; ev fail "$brokerapi"; } >"$WORK/raced.json"
count raced bash "$COUNTER" --report "$WORK/raced.json" ./internal/cipher/api/
judge "$([ "$(code raced)" = 1 ] && said raced 'WARNING: DATA RACE' && said raced 'Write at 0x00c000 by goroutine 7:' &&
  ! said raced 'a line TestPlantedPassBeside printed itself' && echo 0 || echo 1)" \
  "control 3: a data race report filed under a subtest that passed is printed, and a passed test's own output is not" "$(seen raced)" \
  "$(tr '\n' '|' <"$WORK/raced.err")"
# A test the binary runs twice under one name, as internal/cipher/proxy's TestMain runs its
# whole suite once per protocol: test2json reports both runs under that name. The run that failed
# has its failure's lines printed though the other run passed, whichever run failed, and the run
# that passed has none of its own lines printed; read by the name alone, the counter printed none
# of the failure in CI job 135597604679. A test that passed in the first run and was running when
# the binary timed out in the second is unfinished.
pev() { jq -nc --arg p "$brokerapi" --arg o "$1" '{Action: "output", Package: $p, Output: $o}'; }
twice() { # FAILED_RUN: TestPlantedTwice in two runs of one binary, failing in run FAILED_RUN (1 or 2)
  local run
  for run in 1 2; do
    pev "the suite's run $run of 2"$'\n'
    ev run "$brokerapi" TestPlantedTwice; oev TestPlantedTwice '=== RUN   TestPlantedTwice'$'\n'
    if [ "$run" = "$1" ]; then
      oev TestPlantedTwice "    zz_planted_test.go:282: the planted failure of run $run"$'\n'; oev TestPlantedTwice '--- FAIL: TestPlantedTwice (0.00s)'$'\n'
      ev fail "$brokerapi" TestPlantedTwice; pev 'FAIL'$'\n'
    else
      oev TestPlantedTwice 'a line TestPlantedTwice printed in the run that passed'$'\n'; oev TestPlantedTwice '--- PASS: TestPlantedTwice (0.00s)'$'\n'
      ev pass "$brokerapi" TestPlantedTwice; pev 'PASS'$'\n'
    fi
  done
  ev fail "$brokerapi"
}
for failed_run in 1 2; do
  twice "$failed_run" >"$WORK/twice-$failed_run.json"
  count "twice-$failed_run" bash "$COUNTER" --report "$WORK/twice-$failed_run.json" ./internal/cipher/api/
  judge "$([ "$(code "twice-$failed_run")" = 1 ] && said "twice-$failed_run" "FAIL $brokerapi TestPlantedTwice" &&
    said "twice-$failed_run" "zz_planted_test.go:282: the planted failure of run $failed_run" &&
    ! said "twice-$failed_run" 'a line TestPlantedTwice printed in the run that passed' && echo 0 || echo 1)" \
    "control 3: a test run twice under one name that fails in run $failed_run and passes in the other has its failure's lines printed, and none of the passing run's own" \
    "$(seen "twice-$failed_run")" "$(tr '\n' '|' <"$WORK/twice-$failed_run.err")"
done
{ ev run "$brokerapi" TestPlantedSlow; oev TestPlantedSlow '=== RUN   TestPlantedSlow'$'\n'; oev TestPlantedSlow '--- PASS: TestPlantedSlow (0.00s)'$'\n'
  ev pass "$brokerapi" TestPlantedSlow; pev 'PASS'$'\n'
  ev run "$brokerapi" TestPlantedSlow; oev TestPlantedSlow '=== RUN   TestPlantedSlow'$'\n'; oev TestPlantedSlow 'panic: test timed out after 1s'$'\n'
  ev fail "$brokerapi"; } >"$WORK/twice-unfinished.json"
count twice-unfinished bash "$COUNTER" --report "$WORK/twice-unfinished.json" ./internal/cipher/api/
judge "$([ "$(code twice-unfinished)" = 1 ] && said twice-unfinished "UNFINISHED $brokerapi TestPlantedSlow" &&
  said twice-unfinished 'panic: test timed out after 1s' && echo 0 || echo 1)" \
  "control 3: a test that passed in the first run and was running when the second run's binary timed out is printed as unfinished" \
  "$(seen twice-unfinished)" "$(tr '\n' '|' <"$WORK/twice-unfinished.err")"
{ ev pass "$cmdbroker" TestBootBindsOnlyAfterTheCatchUp; ev skip "$cmdbroker" TestPlantedSkip; ev pass "$cmdbroker"; } >"$WORK/spelled.json"
for spelling in ./cmd/./cipherd/ .//cmd/cipherd/ ./cmd/cipherd/../cipherd/; do
  count spelled bash "$COUNTER" --report "$WORK/spelled.json" "$spelling"
  judge "$([ "$(code spelled)" = 1 ] && grep -qxF "SKIP $cmdbroker TestPlantedSkip" "$WORK/spelled.out" &&
    grep -qxF 'database_skips=1' "$WORK/spelled.out" && echo 0 || echo 1)" \
    "control 3: a skip in a package spelled $spelling is counted" "$(seen spelled)"
done
QUIET=(./internal/cipher/api/ ./cmd/cipherd/ ./internal/cipher/proxyrules/)
{ ev pass "$brokerapi" TestHealthzIsPublic; ev pass "$brokerapi"
  ev pass "$cmdbroker" TestBootBindsOnlyAfterTheCatchUp; ev pass "$cmdbroker"
  ev pass "$airlock" TestPlantedAirlockPass; airlock_skips
  ev pass "$airlock"; } >"$WORK/quiet.json"
count quiet bash "$COUNTER" --report "$WORK/quiet.json" "${QUIET[@]}"
judge "$([ "$(code quiet)" = 0 ] && [ "$(out quiet)" = 'database_skips=0' ] && echo 0 || echo 1)" \
  "control 3: a report of the broker's tests whose only skips are the expected ones passes" "$(seen quiet)"
grep -v '"FuzzAirlock"' "$WORK/quiet.json" >"$WORK/stale.json"
count stale bash "$COUNTER" --report "$WORK/stale.json" "${QUIET[@]}"
unknown_report stale "EXPECTED_SKIPS names $airlock FuzzAirlock" \
  "an EXPECTED_SKIPS line whose test a report of its package does not hold is UNKNOWN"

echo "database-skips.test.sh: control 4, --timeout" >&2
plant wait internal/cipher/api "$WAIT_BODY"
wait_run='-run=^(TestPlantedWait|TestPlantedQuick)$'
count timeout env GOFLAGS="-overlay=$WORK/wait.overlay $wait_run" bash "$COUNTER" --timeout 1s ./internal/cipher/api/
judge "$([ "$(code timeout)" = 1 ] && grep -qE '^go_test_exit=[1-9]' "$WORK/timeout.out" &&
  said timeout 'panic: test timed out after 1s' && said timeout "UNFINISHED $MODULE/internal/cipher/api TestPlantedWait" &&
  echo 0 || echo 1)" \
  "control 4: --timeout 1s ends the binary of a package whose test runs three seconds, fails the counter and prints the timeout's panic though a passed test's line came last" "$(seen timeout)"
count no-timeout env GOFLAGS="-overlay=$WORK/wait.overlay $wait_run" bash "$COUNTER" ./internal/cipher/api/
judge "$([ "$(code no-timeout)" = 0 ] && grep -qxF 'database_skips=0' "$WORK/no-timeout.out" &&
  grep -qxF 'go_test_exit=0' "$WORK/no-timeout.out" && echo 0 || echo 1)" \
  "control 4: the same run without --timeout passes" "$(seen no-timeout)"

echo "database-skips.test.sh: control 5, a copy of the counter with one EXPECTED_SKIPS line deleted" >&2
TREE="$WORK/tree"
# The checkout holds the working tree's tracked changes (git stash create, which changes no ref and
# no file; HEAD when there are none), so a test and its EXPECTED_SKIPS line added together and not
# yet committed are both in it. A file git does not track yet is not: `git add` a new test file
# before a local run.
snapshot="$(git -C "$ENVOY" stash create)"
git -C "$ENVOY" worktree add --detach --quiet "$TREE" "${snapshot:-HEAD}"
tree_envoy="$TREE/${ENVOY#"$(git -C "$ENVOY" rev-parse --show-toplevel)/"}"
copy="$tree_envoy/scripts/database-skips.sh"
# The copy is of the counter under test, which a working tree may hold uncommitted.
grep -v "^$API TestSearchLatencyOnCorpus " "$COUNTER" >"$copy"
judge "$([ "$(($(wc -l <"$COUNTER") - $(wc -l <"$copy")))" = 1 ] && echo 0 || echo 1)" \
  "control 5: the copy differs from the counter by exactly the deleted line" \
  "$(($(wc -l <"$COUNTER") - $(wc -l <"$copy"))) lines fewer"
api_run="^($API_ANCHOR|$api_expected)$"
count deleted env GOFLAGS="-run=$api_run" bash "$copy" \
  --var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL ./internal/dispatch/api/
judge "$([ "$(code deleted)" = 1 ] && [ "$(grep -c '^SKIP ' "$WORK/deleted.out")" = 1 ] &&
  grep -qxF "SKIP $API TestSearchLatencyOnCorpus" "$WORK/deleted.out" && echo 0 || echo 1)" \
  "control 5: without its EXPECTED_SKIPS line, TestSearchLatencyOnCorpus's skip is counted" "$(seen deleted)"
count kept env GOFLAGS="-run=$api_run" bash "$COUNTER" \
  --var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL ./internal/dispatch/api/
judge "$([ "$(code kept)" = 0 ] && grep -qxF 'database_skips=0' "$WORK/kept.out" && echo 0 || echo 1)" \
  "control 5: the counter itself passes the same run, the table's tests skipping as expected" "$(seen kept)"
noreason="$tree_envoy/scripts/database-skips-noreason.sh"
sed "s|^\($API TestSearchLatencyOnCorpus\) .*|\1|" "$COUNTER" >"$noreason"
count noreason bash "$noreason" --report "$WORK/quiet.json" "${QUIET[@]}"
judge "$([ "$(code noreason)" = 1 ] && said noreason "EXPECTED_SKIPS line \"$API TestSearchLatencyOnCorpus\" gives no reason" && echo 0 || echo 1)" \
  "control 5: a copy whose EXPECTED_SKIPS line gives no reason is refused when it starts" "$(seen noreason)"
# Each of the two variables, unset and then empty, beside the other set: a counter that kept only
# the first or only the last --var fails one of these.
for name in DISPATCH_TEST_DATABASE_URL BROKER_TEST_DATABASE_URL; do
  for how in unset empty; do
    if [ "$how" = unset ]; then drop=(-u "$name"); else drop=("$name="); fi
    count "var-$how" env "${drop[@]}" GOFLAGS="-run=$api_run" bash "$COUNTER" \
      --var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL ./internal/dispatch/api/
    judge "$([ "$(code "var-$how")" = 1 ] && [ "$(out "var-$how")" = 'database_skips=UNKNOWN' ] &&
      said "var-$how" "$name is unset or empty" && echo 0 || echo 1)" \
      "control 5: with $name $how beside the other variable set, the counter prints UNKNOWN naming it and runs no test" "$(seen "var-$how")"
  done
done

echo "database-skips.test.sh: control 6, --count-table" >&2
# A unit leg's packages run in the counter's one go test, as envoy-go's unit legs run them: the
# table's packages among them are counted, a skip of any other is not, a failure of any fails the
# counter, and each prints its time. Here the leg is internal/fold, which is no package of the
# table, beside the broker API's package, which is, both narrowed by -run.
fold="$MODULE/internal/fold"
table_run='-run=^(TestPlantedSkip|TestPlantedFail|TestHealthzIsPublic)$'
plant fold-skip internal/fold "$SKIP_BODY"
count table-skip env GOFLAGS="-overlay=$WORK/fold-skip.overlay $table_run" bash "$COUNTER" \
  --count-table ./internal/fold/ ./internal/cipher/api/
judge "$([ "$(code table-skip)" = 0 ] && grep -qxF 'database_skips=0' "$WORK/table-skip.out" &&
  grep -qxF 'go_test_exit=0' "$WORK/table-skip.out" && grep -qE "^pass $fold " "$WORK/table-skip.err" &&
  grep -qE "^pass $MODULE/internal/cipher/api " "$WORK/table-skip.err" && echo 0 || echo 1)" \
  "control 6: a skip planted in a package of the run outside the table is not counted, and both packages' times are printed" \
  "$(seen table-skip); stderr $(tr '\n' '|' <"$WORK/table-skip.err")"
plant api-skip internal/cipher/api "$SKIP_BODY"
count table-counted env GOFLAGS="-overlay=$WORK/api-skip.overlay $table_run" bash "$COUNTER" \
  --count-table ./internal/fold/ ./internal/cipher/api/
judge "$([ "$(code table-counted)" = 1 ] && grep -qxF "SKIP $MODULE/internal/cipher/api TestPlantedSkip" "$WORK/table-counted.out" &&
  grep -qxF 'database_skips=1' "$WORK/table-counted.out" && echo 0 || echo 1)" \
  "control 6: a skip planted in the table's package among the run's is counted, and fails the counter" "$(seen table-counted)"
plant fold-fail internal/fold 'func TestPlantedFail(t *testing.T) { t.Fatal("planted failure") }'
count table-fail env GOFLAGS="-overlay=$WORK/fold-fail.overlay $table_run" bash "$COUNTER" \
  --count-table ./internal/fold/ ./internal/cipher/api/
judge "$([ "$(code table-fail)" = 1 ] && grep -qxF 'database_skips=0' "$WORK/table-fail.out" &&
  grep -qE '^go_test_exit=[1-9]' "$WORK/table-fail.out" && said table-fail "FAIL $fold TestPlantedFail" &&
  said table-fail 'planted failure' && echo 0 || echo 1)" \
  "control 6: a failure planted in a package of the run outside the table fails the counter, which prints it" "$(seen table-fail)"
# A leg that holds no package of the table counts nothing: it says so, rather than read as a
# count of zero, and go test's result alone decides.
count table-none env GOFLAGS="-overlay=$WORK/fold-skip.overlay $table_run" bash "$COUNTER" \
  --count-table ./internal/fold/
judge "$([ "$(code table-none)" = 0 ] && grep -qxF 'database_skips=NONE' "$WORK/table-none.out" &&
  grep -qxF 'go_test_exit=0' "$WORK/table-none.out" && said table-none 'so this run counts no skip' &&
  echo 0 || echo 1)" \
  "control 6: a run with no package of the table prints database_skips=NONE, not a zero, and passes on go test's pass" "$(seen table-none)"
count table-none-fail env GOFLAGS="-overlay=$WORK/fold-fail.overlay $table_run" bash "$COUNTER" \
  --count-table ./internal/fold/
judge "$([ "$(code table-none-fail)" = 1 ] && grep -qxF 'database_skips=NONE' "$WORK/table-none-fail.out" &&
  grep -qE '^go_test_exit=[1-9]' "$WORK/table-none-fail.out" && said table-none-fail "FAIL $fold TestPlantedFail" &&
  echo 0 || echo 1)" \
  "control 6: a run with no package of the table still fails on a failed test, which it prints" "$(seen table-none-fail)"
count table-report bash "$COUNTER" --report "$WORK/quiet.json" --count-table "${QUIET[@]}"
judge "$([ "$(code table-report)" = 1 ] && said table-report 'refused beside it' && echo 0 || echo 1)" \
  "control 6: --count-table is refused beside --report" "$(seen table-report)"
count table-bare bash "$COUNTER" --count-table
judge "$([ "$(code table-bare)" = 1 ] && said table-bare 'needs a PACKAGE' && echo 0 || echo 1)" \
  "control 6: --count-table with no package is refused" "$(seen table-bare)"

echo "database-skips.test.sh: control 7, -run (a leg's group)" >&2
# A leg of envoy-go-race-api pipes `-run=^(Name|...)$` and the package into the counter. Each run
# here names the counter's own -run, not GOFLAGS', and runs the API package.
leg_run="-run=^($API_ANCHOR|TestSearchLatencyOnCorpus)\$"
count leg bash "$COUNTER" --var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL \
  "$leg_run" ./internal/dispatch/api/
judge "$([ "$(code leg)" = 0 ] && grep -qxF 'database_skips=0' "$WORK/leg.out" && echo 0 || echo 1)" \
  "control 7: under a -run, an EXPECTED_SKIPS line whose test the package has and the pattern leaves out is another leg's, and the run passes" \
  "$(seen leg)"
# A line whose test the package does not have is held in every leg: no pattern selects it, and a
# counter that dropped every line the pattern leaves out would let it go stale in all of them.
stale="$tree_envoy/scripts/database-skips-stale.sh"
sed "s|^\($API TestSearchLatencyOnCorpus .*\)|\1\n$API TestNoSuchTestAnywhere is planted here, a test the package does not have|" \
  "$COUNTER" >"$stale"
judge "$([ "$(($(wc -l <"$stale") - $(wc -l <"$COUNTER")))" = 1 ] && echo 0 || echo 1)" \
  "control 7: the stale copy differs from the counter by exactly the planted line" \
  "$(($(wc -l <"$stale") - $(wc -l <"$COUNTER"))) lines more"
count leg-stale bash "$stale" --var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL \
  "-run=^($API_ANCHOR)\$" ./internal/dispatch/api/
judge "$([ "$(code leg-stale)" = 1 ] && grep -qxF 'database_skips=UNKNOWN' "$WORK/leg-stale.out" &&
  said leg-stale "TestNoSuchTestAnywhere" && echo 0 || echo 1)" \
  "control 7: under a -run, an EXPECTED_SKIPS line whose test the package does not have still reads UNKNOWN and fails" \
  "$(seen leg-stale)"
judge "$( ! said leg-stale "TestSearchLatencyOnCorpus" && echo 0 || echo 1)" \
  "control 7: and it names only that line, not the lines whose tests the package has and the pattern leaves out" \
  "$(seen leg-stale)"
for shape in '-run=^(.*)$' '-run=^(TestA)|(TestB)$' '-run=TestA'; do
  count leg-shape bash "$COUNTER" --var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL \
    "$shape" ./internal/dispatch/api/
  judge "$([ "$(code leg-shape)" = 1 ] && said leg-shape 'usage:' && echo 0 || echo 1)" \
    "control 7: $shape, no list of whole test names, is refused before any test runs" "$(seen leg-shape)"
done
# A name with a non-ASCII letter is a whole test name: go_legs.py lists and places one, so the
# counter must take it too. It matches no test here, and the anchor still runs.
count leg-unicode bash "$COUNTER" --var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL \
  "-run=^($API_ANCHOR|TestÄnderungGibtEsNicht)\$" ./internal/dispatch/api/
judge "$([ "$(code leg-unicode)" = 0 ] && ! said leg-unicode 'usage:' && grep -qxF 'database_skips=0' "$WORK/leg-unicode.out" &&
  echo 0 || echo 1)" \
  "control 7: a -run name holding a non-ASCII letter is taken, as go_legs.py takes it" "$(seen leg-unicode)"
count leg-report bash "$COUNTER" --report "$WORK/quiet.json" "-run=^($API_ANCHOR)\$" "${QUIET[@]}"
judge "$([ "$(code leg-report)" = 1 ] && said leg-report 'refused beside it' && echo 0 || echo 1)" \
  "control 7: -run is refused beside --report" "$(seen leg-report)"

echo "database-skips.test.sh: control 8, a control that passes prints no test failure" >&2
# probe <0 for a pass> <what was seen> [<what the planted tests printed>]: one judgement by judge, in
# a subshell, so that it is not counted in this run: what judge printed on both streams, then
# `counted <p> passed, <f> failed`, what it added to each count. The evidence of these judgements
# names only counts, verdicts and test names, since it must hold no MARKER itself.
probe() {
  local passed="$PASSED" failed="$FAILED"
  (judge "$1" probe "$2" ${3+"$3"} 2>&1; echo "counted $((PASSED - passed)) passed, $((FAILED - failed)) failed")
}
# Each failure marker, as the first line a run printed, as a later one and as its stderr, fails a
# control whose property held, and is counted as a failure; evidence of the shapes the passing
# controls print (a skip, a count, a package's time) passes, and is counted as a pass.
missed=0
tried=0
for marker in '--- FAIL: TestPlantedMarker (0.00s)' 'FAIL' "FAIL $brokerapi TestPlantedMarker" \
  "UNFINISHED $brokerapi TestPlantedMarker" 'panic: test timed out after 1s' 'WARNING: DATA RACE'; do
  for evidence in "exit 1, printed $marker|database_skips=0|" "exit 1, printed database_skips=0|$marker|" \
    "exit 1, printed database_skips=0|; stderr $marker|"; do
    tried=$((tried + 1))
    [[ "$(probe 0 "$evidence")" == 'FAIL: probe '*$'\ncounted 0 passed, 1 failed' ]] || missed=$((missed + 1))
  done
done
judge "$missed" \
  "control 8: a control whose property held fails, and is counted as failed, when what it saw holds a test failure, for each marker wherever a run's printed lines put it" \
  "$missed of $tried missed"
flagged=0
tried=0
for evidence in "exit 1, printed SKIP $cmdbroker TestPlantedSkip|database_skips=1|" 'exit 0, printed database_skips=0|go_test_exit=0|' \
  "exit 0, printed database_skips=NONE|go_test_exit=0|; stderr pass $fold 0.002s|"; do
  tried=$((tried + 1))
  [[ "$(probe 0 "$evidence")" == 'PASS: probe '*$'\ncounted 1 passed, 0 failed' ]] || flagged=$((flagged + 1))
done
judge "$flagged" "control 8: evidence of a skip, a count or a package's time fails no control" "$flagged of $tried failed"
# A planted failure as the control's planted output: printed on neither stream beside a pass, and
# printed beside a fail.
planted_failure='--- FAIL: TestPlantedMarker (0.00s)|FAIL|'
beside_pass="$(probe 0 'exit 1, printed database_skips=0|' "$planted_failure")"
beside_fail="$(probe 1 'exit 1, printed database_skips=0|' "$planted_failure")"
judge "$([[ "$beside_pass" == 'PASS: probe '*$'\ncounted 1 passed, 0 failed' ]] && ! grep -qE -e "$MARKER" <<<"$beside_pass" && echo 0 || echo 1)" \
  "control 8: a control that passes prints none of its planted tests' failure, on either stream" "judge's verdict ${beside_pass%%:*}"
judge "$([[ "$beside_fail" == 'FAIL: probe '*"$planted_failure"*$'\ncounted 0 passed, 1 failed' ]] && echo 0 || echo 1)" \
  "control 8: a control that fails prints its planted tests' failure" "judge's verdict ${beside_fail%%:*}"
# A log holding one real test's failure as go test prints it, and judge's lines for those two
# controls: a grep of it for `--- FAIL: <name>` finds the real test by its own name, and the
# planted one only as TestPlanted..., on the one line of the control that failed.
log="$(printf '%s\n' '--- FAIL: TestTheRealFailure (0.00s)' "$beside_pass" "$beside_fail")"
found="$(grep -oE -e '--- FAIL: [A-Za-z0-9_]+' <<<"$log" | awk '{ print $NF }' | sort | paste -sd ' ' || true)"
judge "$([ "$found" = 'TestPlantedMarker TestTheRealFailure' ] && [ "$(grep -cF -e '--- FAIL: TestPlanted' <<<"$log")" = 1 ] &&
  grep -F -e '--- FAIL: TestPlanted' <<<"$log" | grep -q '^FAIL: probe ' && echo 0 || echo 1)" \
  "control 8: a grep of a log for failed tests finds a real failure by its own name, and a planted one only as TestPlanted..., beside a control that failed" \
  "found $found"
# failed_tests FILE: the failed and unfinished tests FILE holds, one per line, as go test prints one
# (`--- FAIL: <name>`) and as the counter does (`FAIL <package> <name>`, `UNFINISHED <package>
# <name>`), read as text whatever bytes FILE holds; it returns 2 when it cannot read FILE.
failed_tests() {
  local lines status=0
  lines="$(grep -aoE -e '--- FAIL: [^ ]+|^(FAIL|UNFINISHED) [^ ]+ [^ ]+' "$1")" || status=$?
  [ "$status" -le 1 ] || return 2
  [ -z "$lines" ] || awk '{ print $NF }' <<<"$lines" | sort -u
}
# failed_tests itself: beside a planted failure, a real test's failure in each of the three forms,
# after a NUL byte, is found by its name, and go test's package line and a bare FAIL name none; and
# a file it cannot read is no read at all.
scan_missed=0
for form in '--- FAIL: TestTheRealFailure (0.00s)' "FAIL $brokerapi TestTheRealFailure" "UNFINISHED $brokerapi TestTheRealFailure"; do
  printf 'FAIL %s TestPlantedMarker\noutput\0with a NUL byte\n%s\nFAIL\t%s\t0.012s\nFAIL\n' \
    "$brokerapi" "$form" "$brokerapi" >"$WORK/scan-probe.txt"
  [ "$(failed_tests "$WORK/scan-probe.txt" | paste -sd ' ')" = 'TestPlantedMarker TestTheRealFailure' ] || scan_missed=$((scan_missed + 1))
done
scan_status=0
failed_tests "$WORK/no-such-file.txt" >/dev/null 2>&1 || scan_status=$?
[ "$scan_status" = 2 ] || scan_missed=$((scan_missed + 1))
judge "$scan_missed" \
  "control 8: the scan finds a real test's failure beside a planted one in each form it is printed in, past a NUL byte, takes no name from a package's FAIL line, and reads an unreadable file as no read" \
  "$scan_missed of 4 missed"
# The failed and unfinished tests that every run of controls 1 to 7 printed (runs.log) are all
# tests this script planted, so a grep for failed tests tells each one a failing control prints
# from a real failure. Controls 3, 4 and 6 print planted failures, so a read that found none read
# nothing, and fails, as does a read that could not read runs.log.
scan_status=0
printed="$(failed_tests "$WORK/runs.log")" || scan_status=$?
printed="$(paste -sd ' ' <<<"$printed")"
unplanted="$(tr ' ' '\n' <<<"$printed" | grep -v '^TestPlanted' | paste -sd ' ' || true)"
judge "$([ "$scan_status" = 0 ] && [ -n "$printed" ] && [ -z "$unplanted" ] && echo 0 || echo 1)" \
  "control 8: every failed or unfinished test that any run of controls 1 to 7 printed is named TestPlanted..., and they printed some" \
  "runs.log $([ "$scan_status" = 0 ] && echo read || echo unreadable); tests: ${printed:-none}; not named TestPlanted...: ${unplanted:-none}"

if [ "$FAILED" -ne 0 ]; then
  echo "database-skips.test.sh: failed ($FAILED of $((PASSED + FAILED)))"
  exit 1
fi
echo "database-skips.test.sh: passed"
