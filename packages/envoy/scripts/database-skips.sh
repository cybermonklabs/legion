#!/usr/bin/env bash
# packages/envoy/scripts/database-skips.sh
#
# The one skip counter of the envoy module's database-backed packages. go test counts a skipped
# test as a pass and, without -v, prints nothing for it, so a package run with its database
# variable unset, or with a t.Skip left in one of its tests, reads as green. This script runs
# `go test -json` and counts the skips in that report, and fails on any.
#
#   database-skips.sh [--var NAME]... [--race] [--timeout D] [--skip PATTERN] [--count-table] [-run=PATTERN] [PACKAGE...]
#   database-skips.sh --report FILE [PACKAGE...]
#
# PACKAGE is a pattern under packages/envoy: `./dir/`, that package alone, or `./dir/...`, it
# and every package under it. With none it counts DEFAULT_PACKAGES below, the broker's packages,
# its command and the writer hold's client, which need BROKER_TEST_DATABASE_URL, less the packages
# DEFAULT_EXCEPT takes out of them. From packages/envoy it runs one
#
#   go test -json -count=1 [-race] [-skip PATTERN] [-run PATTERN] -timeout D PACKAGE...
#
# and reads that report. `--var NAME` names a variable the packages need, and repeats; with none
# it is BROKER_TEST_DATABASE_URL. When a named variable is unset or empty it prints
# `database_skips=UNKNOWN` naming it and exits 1, and runs no test. `--race` adds -race.
# `--timeout D` sets the -timeout, and without it the -timeout is ${GO_TEST_TIMEOUT:-15m}. The
# -timeout bounds each package's test binary, not the run, since go test runs the packages
# several at a time, so a caller that must bound the run sets a limit of its own (CI's steps do).
# `--skip PATTERN` is go test's -skip, for a test another job runs in the same build: envoy-go's
# unit legs leave out the backend count's tests, which envoy-go-skips runs, the rows whose bound is
# on time, which envoy-go-alone runs, and the web display fixture's two, which envoy-go-build runs.
# go test reports nothing for a test -skip leaves out, so it is neither run twice nor counted as a
# skip here.
# `-run=PATTERN` is go test's -run, in the one shape .github/scripts/go_legs.py prints for a leg
# that runs a group of a package's tests, `^(Name|Name|...)$`, each alternative a whole test
# name: the leg pipes it in with its package, so this script hands it to its go test. An
# EXPECTED_SKIPS line whose test the package still has and the pattern leaves out is another
# leg's, and is not held here; without that, a leg running a third of a package would be refused
# with `database_skips=UNKNOWN` for every expected skip the other two thirds hold. A line whose
# test the package no longer has (`go test -list .` under the run's flags) is held in every leg,
# so a deleted or renamed test's line still reads UNKNOWN, as in a run of the whole package. Any
# other -run shape is refused rather than read, since this script reads the names off the
# pattern's own text and go test runs the real pattern through Go's regexp.
# `--count-table` runs every PACKAGE and counts the skips of DEFAULT_PACKAGES' packages (less
# DEFAULT_EXCEPT) among them alone: envoy-go's unit legs run their packages so
# (.github/go-legs.yaml), each leg a share of the module, and the broker's packages share a leg's
# -p slots with the rest of it. The other packages' skips are not counted, since a package outside
# the table skips for reasons this table does not hold, and they are not held to showing a run; a
# failure of theirs fails go test and so this script. A run whose PACKAGEs hold no package of the
# table counts nothing, and says so: it prints `database_skips=NONE` and passes or fails on go
# test's result alone. It needs a PACKAGE.
# `--report FILE` reads FILE instead and runs nothing, which the controls use; the six options
# that shape a run (--var, --race, --timeout, --skip, --count-table, -run) are refused beside it.
#
# It prints each skipped test of a package the patterns name (as `go list` names them under the
# run's own flags) as `SKIP PACKAGE TEST`, then `database_skips=N`, and for a run of its own
# `go_test_exit=E`, go test's own exit status. A report that shows no run is no count at all: it
# prints `database_skips=UNKNOWN` and names what it could not read, rather than a zero. That is a
# report that is missing or empty, or that holds no test that passed or failed of a listed
# package with test files (a package whose tests all skipped, or whose TestMain returned before
# it ran them); and an EXPECTED_SKIPS line whose package is listed and whose test the report does
# not hold, since that line then excuses nothing it can name. It exits 0 only when every listed
# package with test files shows tests run, none of them skipped but those EXPECTED_SKIPS names
# and none failed, and, for a run of its own, go test passed; 1 otherwise. A run of its own also
# prints each package's `pass|fail PACKAGE <seconds>s` on stderr. On a failure it prints,
# on stderr, each failed test, each test a failed package never finished, the failed packages'
# output (a failed test's own lines, a timed-out binary's goroutine dump, a data race, and any line
# test2json files under a test after that test's own `--- PASS` or `--- SKIP`), go test's build
# output and, for a run of its own, its stderr. Each of those is read per run of a test, so a test
# a binary runs twice under one name - with -count, or a TestMain that calls m.Run twice - that
# fails in one run and passes in the other still has its failure's lines printed.
set -euo pipefail
# sort and comm compare the test lists under one collation, whatever the caller's locale.
export LC_ALL=C

# A relative --report is the caller's path; every go command runs from packages/envoy.
caller="$PWD"
cd "$(dirname "$0")/.."

# The broker's packages and its command, less DEFAULT_EXCEPT: every package of each opens the
# broker's Postgres, named by BROKER_TEST_DATABASE_URL, or skips. DEFAULT_EXCEPT is a `grep -Ev`
# pattern over the import paths `go list` prints for DEFAULT_PACKAGES, and a run with no PACKAGE
# runs the packages it keeps. It takes out the race lane's packages under the broker's tree (each
# a package of ENVOY_RACE_LANE in .github/scripts/ci_job_reads.py, which builds the same pattern
# as DATABASE_SKIPS_PATTERN and refuses this script when DEFAULT_EXCEPT says otherwise): the auth
# service's natscallout, the one today, which envoy-go-race runs under the detector, so a run here
# would be its second in the same CI run. One of its tests opens the broker's Postgres, and that
# job runs the package through this script too, so its skips are counted there.
DEFAULT_PACKAGES=(./internal/cipher/... ./cmd/cipherd/)
DEFAULT_EXCEPT='/internal/cipher/natscallout$'

# EXPECTED_SKIPS: PACKAGE TEST REASON, one per line: a test that skips in every run that sets the
# database variables, and why. A skip of a test it names is not counted; a line whose package is
# under a run's patterns names a test that run must hold, and a line with no reason is refused
# when the script starts. A test that skips on some servers only is not here: its skip is counted
# where CI's server runs it (TestAdvancesListEachPriorityOnceHoweverTheDatabaseCollatesTheIDs
# needs a PostgreSQL built with ICU, which CI's image is).
EXPECTED_SKIPS='github.com/sjawhar/envoy/internal/cipher/proxyrules FuzzAirlock needs AIRLOCK_MODEL, a built Lean model of the airlock, which the airlock-lean job builds and runs it with
github.com/sjawhar/envoy/internal/cipher/proxyrules TestModelAgreesOnHostShapes needs AIRLOCK_MODEL, as FuzzAirlock does
github.com/sjawhar/envoy/internal/cipher/proxyrules TestTheModelRefusesAHostlessEntry needs AIRLOCK_MODEL, as FuzzAirlock does
github.com/sjawhar/envoy/internal/dispatch/api TestDismissedScaleHoldsTheInboxsReads is a measurement, run only with LEGION_SCALE_TEST=1
github.com/sjawhar/envoy/internal/dispatch/api TestNearCopyRefusalReplay needs DISPATCH_NEARCOPY_CORPUS, a corpus scripts/near-copy-replay.sh writes
github.com/sjawhar/envoy/internal/dispatch/api TestRelatedRecallOnDuplicates needs DISPATCH_RECALL_CORPUS, a corpus scripts/related-recall.sh writes
github.com/sjawhar/envoy/internal/dispatch/api TestRelatedCheckLatencyOnRecallCorpus needs DISPATCH_RECALL_CORPUS and DISPATCH_RECALL_LATENCY_ISSUE
github.com/sjawhar/envoy/internal/dispatch/api TestSearchLatencyOnCorpus needs DISPATCH_BENCH_DATABASE_URL, a restored corpus copy
github.com/sjawhar/envoy/internal/dispatch/api TestStatementTimings is a measurement on the seeded database DISPATCH_BENCH_DATABASE_URL names (benchtime.Open), which CI leaves unset
github.com/sjawhar/envoy/internal/dispatch/api TestRelatedTermsRefreshTiming is a measurement on that same seeded database (benchtime.Open), run only with DISPATCH_BENCH_TIMING=1 on a quiet machine
github.com/sjawhar/envoy/internal/dispatch/api TestGrowsLinearlyRejectsQuadraticFinalRungHelper is a subprocess helper, which its parent test starts with GROWS_LINEARLY_SLOW_FIRST set
github.com/sjawhar/envoy/internal/dispatch/api TestTheLegionReadMatchesACapturedDaemonState needs LEGION_STATE_CAPTURE, a capture of GET /legion/v1/state from a Legion daemon, which CI does not have'

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

report=""
go_test_exit=""

# by_run is the report's events in order, each event of a test given `_run`: its package, its name
# and which run of it this is. A test binary can run a test more than once under one name (-count,
# or a TestMain that calls m.Run twice, as internal/cipher/proxy's does, once per protocol),
# and test2json reports every run under that one name, so what a run did is read by that key and
# not by the name: read by the name, a test that failed in one run and passed in the other counted
# as passed, and the failure's own lines were left out.
# shellcheck disable=SC2016 # $e is jq's variable, not the shell's
by_run='def by_run: foreach .[] as $e ({};
  if $e.Test != null and $e.Action == "run" then .["\($e.Package) \($e.Test)"] += 1 else . end;
  if $e.Test == null then $e else $e + {_run: "\($e.Package) \($e.Test) \(.["\($e.Package) \($e.Test)"] // 0)"} end);'

# failures prints, on stderr, what a failed run left in the report: each failed test, each run of a
# test a failed package started and never ended (the one a timed-out binary was running), every
# output line of a failed package but those of the runs of its tests that passed or skipped, every
# line filed under a run of a test after that run's own `--- PASS` or `--- SKIP` line, and every
# line from the package's first `panic: ` on (so the goroutine dump of a timeout, a data race and
# the package's own FAIL line), the build output, and, for a run of its own, go test's stderr.
failures() {
  jq -rs "$by_run"' [by_run] as $events
    | (reduce ($events[] | select(.Test != null and (.Action == "pass" or .Action == "fail" or .Action == "skip"))) as $e ({}; .[$e._run] = true)) as $ended
    | (reduce ($events[] | select(.Action == "fail" and .Test == null)) as $e ({}; .[$e.Package] = true)) as $failed
    | $events[] | select(.Test != null)
    | if .Action == "fail" then "FAIL \(.Package) \(.Test)"
      elif .Action == "run" and $failed[.Package] and ($ended[._run] | not) then "UNFINISHED \(.Package) \(.Test)"
      else empty end' "$report" >&2 || true
  # test2json files each output line under the test whose framing line came last. A test's own
  # output comes before its `--- PASS` or `--- SKIP` line, so a line filed under it after that line
  # is another's: a data race report after a subtest passed, or a panic after a parallel test
  # passed. Those lines are printed, as is every line of a failed package from its first `panic: `
  # on, whatever test they are filed under.
  jq -rsj "$by_run"' [by_run] as $events
    | (reduce ($events[] | select(.Test != null and (.Action == "pass" or .Action == "skip"))) as $e ({}; .[$e._run] = true)) as $passed
    | (reduce ($events[] | select(.Action == "fail" and .Test == null)) as $e ({}; .[$e.Package] = true)) as $failed
    | foreach ($events[] | select(.Action == "output" and $failed[.Package // ""])) as $e
        ({panicked: {}, ended: {}, show: false};
         ($e.Output | startswith("panic: ")) as $panic
         | .show = ($panic or .panicked[$e.Package] == true or ($e._run != null and .ended[$e._run] == true))
         | if $panic then .panicked[$e.Package] = true else . end
         | if $e._run != null and ($e.Output | test("^\\s*--- (PASS|SKIP): ")) then .ended[$e._run] = true else . end;
         [.show, $e])
    | .[0] as $misfiled | .[1]
    | select($misfiled or .Test == null or ($passed[._run] | not))
    | .Output' "$report" >&2 || true
  jq -rj 'select(.Action == "build-output") | .Output' "$report" >&2 || true
  [ -z "$go_test_exit" ] || cat "$work/stderr" >&2
}

unknown() {
  echo "database_skips=UNKNOWN"
  echo "database-skips.sh: $1, so no skip was counted" >&2
  if [ -n "$go_test_exit" ]; then
    echo "go_test_exit=$go_test_exit"
    [ "$go_test_exit" -eq 0 ] || failures
  fi
  exit 1
}

usage() {
  echo "database-skips.sh: $1 (usage: database-skips.sh [--var NAME]... [--race] [--timeout D] [--skip PATTERN] [--count-table] [-run=PATTERN] [PACKAGE...], or --report FILE [PACKAGE...])" >&2
  exit 1
}

while IFS= read -r line; do
  [ -n "$line" ] || continue
  read -r _ _ reason <<<"$line"
  [ -n "$reason" ] || unknown "EXPECTED_SKIPS line \"$line\" gives no reason it skips"
done <<<"$EXPECTED_SKIPS"

vars=()
race=()
skip=()
run=()
timeout=""
count_table=""
packages=()
while [ $# -gt 0 ]; do
  case "$1" in
    --var)
      [[ "${2:-}" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || usage "--var needs a variable name, not \"${2:-}\""
      vars+=("$2")
      shift 2
      ;;
    --race)
      race=(-race)
      shift
      ;;
    --timeout)
      [ -n "${2:-}" ] || usage "--timeout needs a duration"
      timeout="$2"
      shift 2
      ;;
    --skip)
      [ -n "${2:-}" ] || usage "--skip needs a go test -skip pattern"
      skip=(-skip "$2")
      shift 2
      ;;
    --count-table)
      count_table=yes
      shift
      ;;
    --report)
      [ -n "${2:-}" ] || usage "--report needs the go test -json report to read"
      case "$2" in
        /*) report="$2" ;;
        *) report="$caller/$2" ;;
      esac
      shift 2
      ;;
    -run=*)
      pattern="${1#-run=}"
      case "$pattern" in
        '^('*')$') ;;
        *) usage "-run takes the anchored alternation go_legs.py prints, ^(Name|Name|...)\$, not \"$pattern\"" ;;
      esac
      inner="${pattern#^(}"
      inner="${inner%)$}"
      [ -n "$inner" ] || usage "-run names no test"
      printf '%s\n' "${inner//|/$'\n'}" | sort -u >"$work/selected"
      # Each alternative a whole test name, by the rule .github/scripts/go_legs.py's TEST_NAME
      # writes them under: Test, Fuzz or Example, then no whitespace and no regex metacharacter.
      # `^(.*)$` or `^(A)|(B)$` is no list of names, and a list this script read off it would not
      # be what go test runs; a name holding a non-ASCII letter is one (its bytes are none of those
      # under LC_ALL=C), as go_legs.py lists and places it.
      test_name='^(Test|Fuzz|Example)[^][:space:].*+?(){}|^$\\[]*$'
      while IFS= read -r name; do
        [[ "$name" =~ $test_name ]] ||
          usage "-run takes whole test names, ^(Name|Name|...)\$, and \"$name\" is none"
      done <"$work/selected"
      run=(-run "$pattern")
      shift
      ;;
    ./*) packages+=("$1"); shift ;;
    *) usage "unknown argument $1: a package is a pattern under packages/envoy that starts with ./" ;;
  esac
done
if [ -n "$report" ] && { [ ${#vars[@]} -gt 0 ] || [ ${#race[@]} -gt 0 ] || [ -n "$timeout" ] || [ ${#skip[@]} -gt 0 ] || [ -n "$count_table" ] || [ ${#run[@]} -gt 0 ]; }; then
  usage "--report reads a run made elsewhere, so --var, --race, --timeout, --skip, --count-table and -run, which shape a run of its own, are refused beside it"
fi
if [ -n "$count_table" ] && [ ${#packages[@]} -eq 0 ]; then
  usage "--count-table counts the table's packages among the packages it is given, so it needs a PACKAGE"
fi
# The default table: the import paths go list prints for DEFAULT_PACKAGES under the run's own
# flags, less DEFAULT_EXCEPT, which the go list and go test below take as they take a pattern.
if [ ${#packages[@]} -eq 0 ] || [ -n "$count_table" ]; then
  go list "${race[@]}" "${DEFAULT_PACKAGES[@]}" >"$work/default" 2>"$work/list" || {
    echo "database_skips=UNKNOWN"
    echo "database-skips.sh: go list ${DEFAULT_PACKAGES[*]} failed, so no test ran and no skip was counted:" >&2
    cat "$work/list" >&2
    exit 1
  }
  # grep -Ev '' would take every line out, since an empty pattern matches all of them, so an
  # empty DEFAULT_EXCEPT keeps the whole table.
  if [ -n "$DEFAULT_EXCEPT" ]; then
    mapfile -t table < <(grep -Ev "$DEFAULT_EXCEPT" "$work/default")
  else
    mapfile -t table <"$work/default"
  fi
  [ ${#table[@]} -gt 0 ] || unknown "DEFAULT_EXCEPT takes every package of ${DEFAULT_PACKAGES[*]} out, so no test ran"
fi
# runs is what is counted, and what the go list below is given: the patterns as written, the
# default table, or with --count-table the table's packages among the patterns' (given is then
# every package the patterns name, which go test runs). The messages name the patterns.
given=()
none=""
if [ ${#packages[@]} -eq 0 ]; then
  packages=("${DEFAULT_PACKAGES[@]}")
  runs=("${table[@]}")
elif [ -n "$count_table" ]; then
  go list "${race[@]}" "${packages[@]}" >"$work/given" 2>"$work/list" || {
    echo "database_skips=UNKNOWN"
    echo "database-skips.sh: go list ${packages[*]} failed, so no test ran and no skip was counted:" >&2
    cat "$work/list" >&2
    exit 1
  }
  mapfile -t given <"$work/given"
  mapfile -t runs < <(printf '%s\n' "${table[@]}" | sort -u | awk 'NR == FNR { held[$0] = 1; next } held[$0]' - "$work/given")
  [ ${#runs[@]} -gt 0 ] || none=yes
else
  runs=("${packages[@]}")
fi
[ ${#vars[@]} -gt 0 ] || vars=(BROKER_TEST_DATABASE_URL)

if [ -z "$report" ]; then
  for name in "${vars[@]}"; do
    [ -n "${!name:-}" ] || unknown "$name is unset or empty, and the packages' database tests skip without it; no test ran"
  done
fi

# The packages the patterns name, as go lists them under the run's own flags (GOFLAGS, -race):
# each by its canonical import path, which is how the report names it whatever spelling the
# pattern used, with how many test files it has.
if [ -n "$none" ]; then
  : >"$work/listed"
else
  go list "${race[@]}" -f '{{.ImportPath}} {{len .TestGoFiles}} {{len .XTestGoFiles}}' "${runs[@]}" >"$work/listed" 2>"$work/list" || {
    echo "database_skips=UNKNOWN"
    echo "database-skips.sh: go list ${packages[*]} failed, so no test ran and no skip was counted:" >&2
    cat "$work/list" >&2
    exit 1
  }
fi
listed="$(jq -R -s -c '[split("\n")[] | select(length > 0) | split(" ") | {(.[0]): true}] | add // {}' "$work/listed")"
# shellcheck disable=SC2016 # $pkg and $listed are jq's variables, not the shell's
under='def under($pkg): $listed[$pkg] == true;'

if [ -z "$report" ]; then
  report="$work/report.json"
  # tested is what go test runs: the counted packages, or with --count-table every package the
  # patterns name, in go list's order.
  tested=("${runs[@]}")
  [ -z "$count_table" ] || tested=("${given[@]}")
  go_test_exit=0
  go test -json -count=1 "${race[@]}" "${skip[@]}" "${run[@]}" -timeout "${timeout:-${GO_TEST_TIMEOUT:-15m}}" "${tested[@]}" >"$report" 2>"$work/stderr" || go_test_exit=$?
fi

[ -e "$report" ] || unknown "the report $report does not exist"
[ -s "$report" ] || unknown "the report $report is empty"
jq -r --argjson listed "$listed" "$under"' select(.Action == "pass" or .Action == "fail" or .Action == "skip") | select(under(.Package // "")) | "\(.Action) \(.Package) \(.Test // "")"' "$report" >"$work/ended" 2>"$work/jq" ||
  unknown "the report $report is not a go test -json stream ($(head -c 200 "$work/jq"))"
status=0
if [ -n "$none" ]; then
  # No package of the table ran, so there is no skip of one to count, and a zero would read as a
  # count that found none: the run says what it is instead.
  echo "database-skips.sh: no package of the table (${DEFAULT_PACKAGES[*]}) is among the ${#given[@]} package(s) given, so this run counts no skip, and go test's result alone decides" >&2
  echo "database_skips=NONE"
else
  # A test that passed or failed is the evidence a package's tests ran: a skip is what this script
  # counts, so a package whose tests all skipped shows none ran, and so does a package whose
  # TestMain returned before it ran them, which passes with no test. Every listed package with a
  # test file must hold one; a package with none has nothing to run.
  awk 'NF == 3 && $1 != "skip" { print $2 }' "$work/ended" | sort -u >"$work/evidenced"
  [ -s "$work/evidenced" ] || unknown "the report $report holds no test of a package under ${packages[*]} that passed or failed"
  awk '$2 + $3 > 0 { print $1 }' "$work/listed" | sort -u | comm -23 - "$work/evidenced" >"$work/silent"
  [ ! -s "$work/silent" ] || unknown "the report $report holds no test that passed or failed in $(paste -sd ',' "$work/silent" | sed 's/,/, /g'), which has test files"

  awk 'NF == 3 && $1 == "skip" { print $2, $3 }' "$work/ended" | sort -u >"$work/skipped"
  awk 'NF == 3 { print $2, $3 }' "$work/ended" | sort -u >"$work/held"
  printf '%s\n' "$EXPECTED_SKIPS" | jq -R -r --argjson listed "$listed" "$under"' select(length > 0) | split(" ") | select(under(.[0])) | "\(.[0]) \(.[1])"' | sort -u >"$work/expected"
  # A run under a -run holds only the tests that pattern selects, so a line whose test the
  # package still has and the pattern leaves out is another leg's, and is dropped here. A line
  # whose test the package no longer has is kept: no pattern selects it, so dropping it in every
  # leg would let a deleted or renamed test's line go stale with nothing to say so. Kept, it is in
  # no report, and the check below names it in every leg, as it does in a run of the whole
  # package. The package's tests are what `go test -list .` prints under the run's own flags, the
  # build the run reuses.
  if [ -s "${work}/selected" ]; then
    go test -list . "${race[@]}" "${runs[@]}" >"$work/tests" 2>"$work/list" || {
      echo "database_skips=UNKNOWN"
      echo "database-skips.sh: go test -list . ${packages[*]} failed, so which EXPECTED_SKIPS lines this -run leaves to another leg is unknown:" >&2
      cat "$work/list" >&2
      exit 1
    }
    grep -E '^(Test|Fuzz|Example)' "$work/tests" | sort -u >"$work/tests.sorted" || true
    awk 'FILENAME == ARGV[1] { selected[$0]; next } FILENAME == ARGV[2] { declared[$0]; next }
         ($2 in selected) || !($2 in declared)' "$work/selected" "$work/tests.sorted" "$work/expected" >"$work/expected.run"
    mv "$work/expected.run" "$work/expected"
  fi
  comm -23 "$work/expected" "$work/held" >"$work/stale"
  [ ! -s "$work/stale" ] || unknown "EXPECTED_SKIPS names $(paste -sd ',' "$work/stale" | sed 's/,/, /g'), under ${packages[*]}, which the report $report does not hold"

  comm -23 "$work/skipped" "$work/expected" >"$work/counted"
  sed 's/^/SKIP /' "$work/counted"
  database_skips="$(wc -l <"$work/counted")"
  echo "database_skips=$database_skips"
  [ "$database_skips" -eq 0 ] || status=1
fi

# A failed test or package fails the count too, in a report read with --report as in a run.
awk '$1 == "fail"' "$work/ended" >"$work/failed"
[ ! -s "$work/failed" ] || status=1
if [ -n "$go_test_exit" ]; then
  # Each package's time, which go test prints without -json and the CI steps' limits are set
  # from, on stderr: every package's go test ran, those the count left out with --count-table too.
  jq -r 'select(.Test == null and (.Action == "pass" or .Action == "fail")) | "\(.Action) \(.Package) \(.Elapsed // 0)s"' "$report" >&2 || true
  echo "go_test_exit=$go_test_exit"
  [ "$go_test_exit" -eq 0 ] || status=1
fi
if [ -s "$work/failed" ] || { [ -n "$go_test_exit" ] && [ "$go_test_exit" -ne 0 ]; }; then
  failures
fi
exit "$status"
