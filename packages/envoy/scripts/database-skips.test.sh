#!/usr/bin/env bash
# packages/envoy/scripts/database-skips.test.sh
#
# Tests database-skips.sh, the skip counter of the envoy module's database-backed packages, with
# five controls. Each control must fail as it states, or this test fails; each beside it a run
# the counter must pass, so a counter that fails everything fails here too:
#
#   1. a skipped test planted in ./internal/broker/api/, ./cmd/broker/ and
#      ./internal/dispatch/api/, one at a time, through a Go overlay (GOFLAGS=-overlay) that adds
#      zz_planted_skip_test.go, is printed as `SKIP <package> TestPlantedSkip` and fails the
#      counter; the same run without the overlay passes;
#   2. with its variable unset or empty, the counter prints `database_skips=UNKNOWN` naming it,
#      runs no test and fails;
#   3. --report of a report that shows no run prints `database_skips=UNKNOWN` and fails: one
#      holding no test of a package the patterns name, one whose only tests there are expected
#      skips, one where a named package with test files passed with no test or is missing, a
#      missing and an empty report, and an EXPECTED_SKIPS line whose test the report does not
#      hold; a report holding a failed test fails; a package spelled other than canonically
#      (`./cmd/./broker/`, `.//cmd/broker/`, `./cmd/broker/../broker/`) is still counted; and a
#      report of the broker's tests whose only skips are the expected ones passes;
#   4. --timeout 1s on ./internal/broker/api/, beside a planted parallel test that runs three
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
#
# Every run narrows its packages to the tests it is about with
# GOFLAGS=-run=..., which go test reads as its own -run (and go list ignores): what the counter
# makes of a report does not depend on how many other tests passed in it, and the API package's
# whole run is the race job's. The ./internal/dispatch/api/ runs name every test EXPECTED_SKIPS
# lists for the package, which the counter holds a run of that package to, and one test that
# passes, since a package whose tests all skip shows no run.
#
# It prints a PASS: or FAIL: line per property and then `database-skips.test.sh: passed` or
# `database-skips.test.sh: failed (<n> of <m>)`, and exits 0 or 1.
#
# Usage: packages/envoy/scripts/database-skips.test.sh
#
# Needs go, jq and git, this repository's git checkout, and a scratch Postgres named by both
# BROKER_TEST_DATABASE_URL and DISPATCH_TEST_DATABASE_URL, as ci.yaml's envoy-go job sets them;
# with either unset it fails, naming it, since every control but 2 and 3 runs database tests.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY="$(cd "$SCRIPT_DIR/.." && pwd)"
COUNTER="$SCRIPT_DIR/database-skips.sh"
MODULE="$(awk '$1 == "module" { print $2; exit }' "$ENVOY/go.mod")"
API="$MODULE/internal/dispatch/api"
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
judge() { # <0 for a pass> <property> <what was seen>
  if [ "$1" = 0 ]; then
    PASSED=$((PASSED + 1))
    printf 'PASS: %s — %s\n' "$2" "$3"
  else
    FAILED=$((FAILED + 1))
    printf 'FAIL: %s — %s\n' "$2" "$3"
  fi
}

for name in BROKER_TEST_DATABASE_URL DISPATCH_TEST_DATABASE_URL; do
  if [ -z "${!name:-}" ]; then
    echo "database-skips.test.sh: $name is unset or empty, so the controls that run database tests cannot run" >&2
    echo "database-skips.test.sh: failed (UNKNOWN: $name)"
    exit 1
  fi
done

# count NAME COMMAND...: runs COMMAND, keeping its stdout, stderr and exit status as NAME.
count() {
  local name="$1"
  shift
  local code=0
  "$@" >"$WORK/$name.out" 2>"$WORK/$name.err" || code=$?
  echo "$code" >"$WORK/$name.exit"
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
airlock="$MODULE/internal/broker/airlock"
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
  package="$(cd "$ENVOY" && go list -f '{{.Name}}' "./$dir/")"
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

echo "database-skips.test.sh: control 1, a planted skip in each of three packages" >&2
# Each package's -run names the planted test and a test the package passes with the database set
# (the API package's, the tests its table excuses), so the run without the overlay holds tests.
for row in "internal/broker/api ^(TestPlantedSkip|TestHealthzIsPublic)$" \
  "cmd/broker ^(TestPlantedSkip|TestBootBindsOnlyAfterTheCatchUp)$" \
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
# airlock_skips: a skip event of each test EXPECTED_SKIPS lists for the airlock package.
airlock_skips() { table_tests "$airlock" | while IFS= read -r test; do ev skip "$airlock" "$test"; done; }
brokerapi="$MODULE/internal/broker/api"
cmdbroker="$MODULE/cmd/broker"
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
{ ev pass "$MODULE/internal/dispatch/store" TestElsewhere; ev skip "$MODULE/internal/dispatch/store" TestSkippedElsewhere
  ev pass "$MODULE/internal/dispatch/store"; } >"$WORK/elsewhere.json"
count elsewhere bash "$COUNTER" --report "$WORK/elsewhere.json"
unknown_report elsewhere 'holds no test of a package under ./internal/broker/... ./cmd/broker/ that passed or failed' \
  "a report holding no test of a package the patterns name is UNKNOWN"
: >"$WORK/empty.json"
count empty-report bash "$COUNTER" --report "$WORK/empty.json"
count missing-report bash "$COUNTER" --report "$WORK/missing.json"
judge "$([ "$(code empty-report)" = 1 ] && [ "$(out empty-report)" = 'database_skips=UNKNOWN' ] &&
  [ "$(code missing-report)" = 1 ] && [ "$(out missing-report)" = 'database_skips=UNKNOWN' ] && echo 0 || echo 1)" \
  "control 3: an empty and a missing report are UNKNOWN" "empty: $(seen empty-report); missing: $(seen missing-report)"
{ airlock_skips; ev pass "$airlock"; } >"$WORK/all-expected.json"
count all-expected bash "$COUNTER" --report "$WORK/all-expected.json" ./internal/broker/airlock/
unknown_report all-expected 'holds no test of a package under ./internal/broker/airlock/ that passed or failed' \
  "a report whose only tests are expected skips is UNKNOWN"
{ ev pass "$brokerapi" TestHealthzIsPublic; ev pass "$brokerapi"; ev pass "$cmdbroker"; } >"$WORK/silent.json"
count silent bash "$COUNTER" --report "$WORK/silent.json" ./internal/broker/api/ ./cmd/broker/
unknown_report silent "holds no test that passed or failed in $cmdbroker, which has test files" \
  "a package that passed with no test, beside one whose tests ran, is UNKNOWN"
{ ev pass "$brokerapi" TestHealthzIsPublic; ev pass "$brokerapi"; } >"$WORK/partial.json"
count partial bash "$COUNTER" --report "$WORK/partial.json" ./internal/broker/api/ ./cmd/broker/
unknown_report partial "holds no test that passed or failed in $cmdbroker, which has test files" \
  "a named package the report does not hold, beside one whose tests ran, is UNKNOWN"
{ ev pass "$brokerapi" TestHealthzIsPublic; ev fail "$brokerapi" TestABrokenTest; ev fail "$brokerapi"; } >"$WORK/failed.json"
count failed bash "$COUNTER" --report "$WORK/failed.json" ./internal/broker/api/
judge "$([ "$(code failed)" = 1 ] && [ "$(out failed)" = 'database_skips=0' ] && said failed "FAIL $brokerapi TestABrokenTest" && echo 0 || echo 1)" \
  "control 3: a report holding a failed test fails, naming it" "$(seen failed)"
# A data race report the detector prints after a subtest's `--- PASS` is filed under that passed
# subtest, as test2json files it; the counter must print it, and no line a passed test printed
# before its own `--- PASS`.
oev() { jq -nc --arg p "$brokerapi" --arg t "$1" --arg o "$2" '{Action: "output", Package: $p, Test: $t, Output: $o}'; }
{ oev TestPassedBeside 'a line TestPassedBeside printed itself'$'\n'; oev TestPassedBeside '--- PASS: TestPassedBeside (0.00s)'$'\n'
  ev pass "$brokerapi" TestPassedBeside
  oev TestRaced/sub '--- PASS: TestRaced/sub (0.00s)'$'\n'; ev pass "$brokerapi" TestRaced/sub
  oev TestRaced/sub 'WARNING: DATA RACE'$'\n'; oev TestRaced/sub 'Write at 0x00c000 by goroutine 7:'$'\n'
  oev TestRaced '    testing.go:1617: race detected during execution of test'$'\n'; oev TestRaced '--- FAIL: TestRaced (0.01s)'$'\n'
  ev fail "$brokerapi" TestRaced; ev fail "$brokerapi"; } >"$WORK/raced.json"
count raced bash "$COUNTER" --report "$WORK/raced.json" ./internal/broker/api/
judge "$([ "$(code raced)" = 1 ] && said raced 'WARNING: DATA RACE' && said raced 'Write at 0x00c000 by goroutine 7:' &&
  ! said raced 'a line TestPassedBeside printed itself' && echo 0 || echo 1)" \
  "control 3: a data race report filed under a subtest that passed is printed, and a passed test's own output is not" "$(seen raced)"
{ ev pass "$cmdbroker" TestBootBindsOnlyAfterTheCatchUp; ev skip "$cmdbroker" TestPlantedSkip; ev pass "$cmdbroker"; } >"$WORK/spelled.json"
for spelling in ./cmd/./broker/ .//cmd/broker/ ./cmd/broker/../broker/; do
  count spelled bash "$COUNTER" --report "$WORK/spelled.json" "$spelling"
  judge "$([ "$(code spelled)" = 1 ] && grep -qxF "SKIP $cmdbroker TestPlantedSkip" "$WORK/spelled.out" &&
    grep -qxF 'database_skips=1' "$WORK/spelled.out" && echo 0 || echo 1)" \
    "control 3: a skip in a package spelled $spelling is counted" "$(seen spelled)"
done
QUIET=(./internal/broker/api/ ./cmd/broker/ ./internal/broker/airlock/)
{ ev pass "$brokerapi" TestHealthzIsPublic; ev pass "$brokerapi"
  ev pass "$cmdbroker" TestBootBindsOnlyAfterTheCatchUp; ev pass "$cmdbroker"
  ev pass "$airlock" TestAnAirlockTestThatRan; airlock_skips
  ev pass "$airlock"; } >"$WORK/quiet.json"
count quiet bash "$COUNTER" --report "$WORK/quiet.json" "${QUIET[@]}"
judge "$([ "$(code quiet)" = 0 ] && [ "$(out quiet)" = 'database_skips=0' ] && echo 0 || echo 1)" \
  "control 3: a report of the broker's tests whose only skips are the expected ones passes" "$(seen quiet)"
grep -v '"FuzzAirlock"' "$WORK/quiet.json" >"$WORK/stale.json"
count stale bash "$COUNTER" --report "$WORK/stale.json" "${QUIET[@]}"
unknown_report stale "EXPECTED_SKIPS names $airlock FuzzAirlock" \
  "an EXPECTED_SKIPS line whose test a report of its package does not hold is UNKNOWN"

echo "database-skips.test.sh: control 4, --timeout" >&2
plant wait internal/broker/api "$WAIT_BODY"
wait_run='-run=^(TestPlantedWait|TestPlantedQuick)$'
count timeout env GOFLAGS="-overlay=$WORK/wait.overlay $wait_run" bash "$COUNTER" --timeout 1s ./internal/broker/api/
judge "$([ "$(code timeout)" = 1 ] && grep -qE '^go_test_exit=[1-9]' "$WORK/timeout.out" &&
  said timeout 'panic: test timed out after 1s' && said timeout "UNFINISHED $MODULE/internal/broker/api TestPlantedWait" &&
  echo 0 || echo 1)" \
  "control 4: --timeout 1s ends the binary of a package whose test runs three seconds, fails the counter and prints the timeout's panic though a passed test's line came last" "$(seen timeout)"
count no-timeout env GOFLAGS="-overlay=$WORK/wait.overlay $wait_run" bash "$COUNTER" ./internal/broker/api/
judge "$([ "$(code no-timeout)" = 0 ] && grep -qxF 'database_skips=0' "$WORK/no-timeout.out" &&
  grep -qxF 'go_test_exit=0' "$WORK/no-timeout.out" && echo 0 || echo 1)" \
  "control 4: the same run without --timeout passes" "$(seen no-timeout)"

echo "database-skips.test.sh: control 5, a copy of the counter with one EXPECTED_SKIPS line deleted" >&2
TREE="$WORK/tree"
# The checkout holds the working tree's tracked changes (git stash create, which changes no ref and
# no file; HEAD when there are none), so a test and its EXPECTED_SKIPS line added together and not
# yet committed are both in it.
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

if [ "$FAILED" -ne 0 ]; then
  echo "database-skips.test.sh: failed ($FAILED of $((PASSED + FAILED)))"
  exit 1
fi
echo "database-skips.test.sh: passed"
