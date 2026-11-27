#!/usr/bin/env bash
# Tests for `go_test_events.py`: the step's log is the one go test prints without -json (or with
# -v), every test's result is in the events file under the step's id, the step's exit status is go
# test's, and a run that kept no event fails, whatever go test's own status.
#
# Each case runs the real script over a scratch Go module whose packages pass, fail, print, log
# long lines, fuzz, time out, exit inside a test and fail to build, and compares what it prints
# with what plain `go test` prints for the same packages, the times in both read as `Ns`. The
# controls run it over a stand-in `go` that writes no event, over scripts that append to
# TEST_EVENTS_FILE or do not, over an events file that keeps nothing, over the real
# backend-count.sh cut short by SIGTERM, and over stand-ins whose events a file size limit cuts
# short, whose events hold one the log's renderer cannot print, and that SIGTERM cuts short.
#
# Its own go test runs are a fixture: they are the runs each case compares the script's with, over
# a module of this harness's making, and no test of this repository, so the CI step that runs this
# harness is no go test step of check-workflow-files.sh's rule 8 (go_test_steps.py's FIXTURES
# names it).
#
# Run from anywhere: .github/scripts/go_test_events.test.sh
# CI runs it in ci.yaml's test job, after its setup-go.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
writer="$script_dir/go_test_events.py"
repo=$(cd "$script_dir/../.." && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
module="$work/module"
# The events directory a runner's step writes to, which the script holds TEST_EVENTS_FILE to.
runner="$work/runner"
events="$runner/go-test-events"
mkdir -p "$events"
for package in pass fail notests leak hang broken print subprint longlog panictext exits fuzz fuzzfail partial partialexit logshape bench spaced spacedexit labels; do
  mkdir -p "$module/$package"
done
export GOWORK=off GOTOOLCHAIN=local GOFLAGS=
printf 'module example.com/events\n\ngo 1.21\n' >"$module/go.mod"
cat >"$module/pass/pass_test.go" <<'GO'
package pass

import "testing"

func TestPlain(t *testing.T) { t.Log("a log line") }

func TestNested(t *testing.T) {
	t.Run("inner", func(t *testing.T) { t.Log("the inner log") })
}

func TestSkipped(t *testing.T) { t.Skip("not on this machine") }
GO
cat >"$module/fail/fail_test.go" <<'GO'
package fail

import "testing"

func TestFine(t *testing.T) { t.Log("fine") }

func TestBroken(t *testing.T) {
	t.Log("before")
	t.Run("passes", func(t *testing.T) { t.Log("a passing subtest's log") })
	t.Run("fails", func(t *testing.T) {
		t.Log("the failing subtest's log")
		t.Run("deeper", func(t *testing.T) { t.Error("the deepest failure") })
	})
	t.Error("the top-level failure")
}
GO
printf 'package notests\n\nfunc X() {}\n' >"$module/notests/x.go"
cat >"$module/leak/leak_test.go" <<'GO'
package leak

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	fmt.Println("leak check: 0 services left alive")
	os.Exit(code)
}

func TestLeak(t *testing.T) {}
GO
cat >"$module/hang/hang_test.go" <<'GO'
package hang

import (
	"testing"
	"time"
)

func TestQuick(t *testing.T) {}

func TestHang(t *testing.T) { time.Sleep(time.Minute) }
GO
cat >"$module/broken/broken_test.go" <<'GO'
package broken

import "testing"

func TestX(t *testing.T) { undefined() }
GO
cat >"$module/print/print_test.go" <<'GO'
package print

import (
	"fmt"
	"testing"
)

func TestPrints(t *testing.T) { fmt.Println("a passing test's own print") }

func TestFails(t *testing.T) {
	fmt.Println("the failing test's own print")
	t.Error("the failure")
}
GO
cat >"$module/subprint/subprint_test.go" <<'GO'
package subprint

import (
	"fmt"
	"testing"
)

func TestParent(t *testing.T) {
	t.Run("child", func(t *testing.T) {
		fmt.Println("the failing subtest's own print")
		t.Error("the child's failure")
	})
}
GO
cat >"$module/longlog/longlog_test.go" <<'GO'
package longlog

import (
	"strings"
	"testing"
)

func TestParent(t *testing.T) {
	t.Run("child", func(t *testing.T) { t.Error(strings.Repeat("a long diagnostic ", 300)) })
}
GO
cat >"$module/panictext/panictext_test.go" <<'GO'
package panictext

import (
	"fmt"
	"testing"
)

func TestPrintsPanicText(t *testing.T) {
	fmt.Println("panic: this is an ordinary diagnostic")
	t.Error("the failure")
}
GO
cat >"$module/exits/exits_test.go" <<'GO'
package exits

import (
	"fmt"
	"os"
	"testing"
)

func TestExits(t *testing.T) {
	t.Log("a log line go test never prints")
	fmt.Println("the test's own print before it exits")
	os.Exit(1)
}
GO
cat >"$module/fuzz/fuzz_test.go" <<'GO'
package fuzz

import "testing"

func FuzzPasses(f *testing.F) {
	f.Add(1)
	f.Fuzz(func(t *testing.T, i int) {})
}
GO
cat >"$module/fuzzfail/fuzzfail_test.go" <<'GO'
package fuzzfail

import "testing"

func FuzzFails(f *testing.F) {
	f.Add(1)
	f.Fuzz(func(t *testing.T, i int) { t.Error("the fuzz failure") })
}
GO
cat >"$module/partial/partial_test.go" <<'GO'
package partial

import (
	"fmt"
	"testing"
)

func TestPartial(t *testing.T) {
	fmt.Print("a print that ends no line")
	t.Log("the buffered log")
	t.Error("the failure")
}
GO
cat >"$module/partialexit/partialexit_test.go" <<'GO'
package partialexit

import (
	"fmt"
	"os"
	"testing"
)

func TestPartialExit(t *testing.T) {
	fmt.Print("a print that ends no line")
	t.Log("a log line go test never prints")
	os.Exit(1)
}
GO
cat >"$module/logshape/logshape_test.go" <<'GO'
package logshape

import (
	"fmt"
	"testing"
)

func TestLogShaped(t *testing.T) {
	fmt.Println("    pretend.go:123: a print laid out as t.Log lays out a line")
	t.Error("the failure")
}

func TestPassingLogShaped(t *testing.T) {
	fmt.Println("    pretend.go:124: a passing test's print laid out the same way")
}
GO
cat >"$module/spaced/spaced_test.go" <<'GO'
package spaced

import (
	"fmt"
	"testing"
)

func TestSpaced(t *testing.T) {
	fmt.Print("status: ")
	t.Log("the buffered log")
	t.Error("the failure")
}
GO
cat >"$module/spacedexit/spacedexit_test.go" <<'GO'
package spacedexit

import (
	"fmt"
	"os"
	"testing"
)

func TestSpacedExit(t *testing.T) {
	fmt.Print("status: ")
	t.Log("a log line go test never prints")
	os.Exit(1)
}
GO
# A test file whose name holds a space, and a //line label that is no .go file: their t.Log
# lines carry those labels.
cat >"$module/labels/with space_test.go" <<'GO'
package labels

import "testing"

func TestSpacedName(t *testing.T) { t.Log("a passing test's log, which go test never prints") }

func TestFailing(t *testing.T) { t.Error("the failure") }
GO
cat >"$module/labels/line_test.go" <<'GO'
package labels

import "testing"

func TestLineLabel(t *testing.T) {
//line generated.tpl:7
	t.Log("a passing test's log under a //line label")
}
GO
cat >"$module/bench/bench_test.go" <<'GO'
package bench

import "testing"

func TestPlain(t *testing.T) {}

func BenchmarkA(b *testing.B) {
	for i := 0; i < b.N; i++ {
	}
}
GO

# The times, rates and counts that differ between two runs of one test, read as one word so the
# runs compare; a benchmark's line pads its numbers to their width, so its blanks are one.
norm() { sed -E 's/[0-9]+\.[0-9]+s/Ns/g; /^(fuzz: |Benchmark)/s/[0-9.]+/N/g; /^Benchmark/s/[[:space:]]+/ /g'; }

# plain [env...] -- <args...>: plain go test over the module (or the directory -C names); its
# stdout, stderr and status.
plain() {
  local env=() dir="$module"
  while [ "$1" != "--" ]; do env+=("$1"); shift; done
  shift
  if [ "${1:-}" = "-C" ]; then dir="$module/$2"; shift 2; fi
  set +e
  (cd "$dir" && env "${env[@]}" go test "$@") >"$work/plain.out" 2>"$work/plain.err"
  plain_status=$?
  set -e
}

# kept <env...> -- <args...>: the script under test over the module (or the directory -C names),
# as a runner's step runs it (RUNNER_TEMP, which a case may override); its stdout, stderr and
# status.
kept() {
  local env=(RUNNER_TEMP="$runner") dir="$module"
  while [ "$1" != "--" ]; do env+=("$1"); shift; done
  shift
  if [ "${1:-}" = "-C" ]; then dir="$module/$2"; shift 2; fi
  set +e
  (cd "$dir" && env "${env[@]}" "$writer" "$@") >"$work/kept.out" 2>"$work/kept.err"
  kept_status=$?
  set -e
}

# compare <name> <args...>: plain go test and the script under test over the same arguments, the
# script's events in <name>.jsonl of the runner's events directory.
compare() {
  local name=$1
  shift
  plain -- "$@"
  kept TEST_EVENTS_FILE="$events/$name.jsonl" -- go test "$@"
}

same() { [ "$(norm <"$1")" = "$(norm <"$2")" ] && echo true || echo false; }

# pass_of <file> <test>: how many pass events of that full test name the file holds.
pass_of() { jq -c --arg t "$2" 'select(.Action == "pass" and .Test == $t)' "$1" | wc -l | tr -d ' '; }

echo "case: packages that pass, skip or have no tests print what go test prints, and every test's result is kept under the step's id"
plain -- -count=1 ./pass/ ./notests/ ./leak/
kept GITHUB_ACTION=unit -- go test -count=1 ./pass/ ./notests/ ./leak/
check "exits as go test does (0)" "$(is "$kept_status" "$plain_status")"
check "its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"
check "its stderr is go test's" "$(same "$work/plain.err" "$work/kept.err")"
check "the log holds no test's own line" "$(is "$(contains "$(cat "$work/kept.out")" '=== RUN\|a log line')" false)"
check "the events are in go-test-events/<step id>.jsonl" "$([ -s "$events/unit.jsonl" ] && echo true || echo false)"
check "a top-level test's pass is kept" "$(is "$(pass_of "$events/unit.jsonl" TestPlain)" 1)"
check "a subtest's pass is kept under its full name" "$(is "$(pass_of "$events/unit.jsonl" TestNested/inner)" 1)"
check "each test's pass and skip event keeps its elapsed seconds and its time" \
  "$(is "$(jq -c 'select(.Test and (.Action == "pass" or .Action == "skip")) | (has("Elapsed") and has("Time"))' "$events/unit.jsonl" | sort -u | paste -sd, -)" true)"
check "a skipped test is kept as a skip, not a pass" \
  "$(is "$(jq -c 'select(.Test == "TestSkipped") | .Action' "$events/unit.jsonl" | sort -u | paste -sd, -)" '"output","run","skip"')"
check "TestMain's own line, which -v alone prints, is kept in the events" \
  "$(contains "$(cat "$events/unit.jsonl")" 'leak check: 0 services left alive')"

echo "case: a second go test of the same step appends to the same file"
before=$(wc -l <"$events/unit.jsonl")
kept GITHUB_ACTION=unit -- go test -count=1 ./leak/
check "exits 0" "$(is "$kept_status" 0)"
check "the file grew" "$([ "$(wc -l <"$events/unit.jsonl")" -gt "$before" ] && echo true || echo false)"
check "and holds both runs of TestLeak" "$(is "$(pass_of "$events/unit.jsonl" TestLeak)" 2)"

echo "case: a failed package prints go test's report of each failed test, subtests nested"
compare fail -count=1 ./fail/ ./pass/
check "exits as go test does (1)" "$(is "$kept_status" "$plain_status")"
check "its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"
check "the failed subtest's report is indented under its parent" \
  "$(contains "$(cat "$work/kept.out")" '^        --- FAIL: TestBroken/fails/deeper (')"
check "a passing subtest's log stays out" "$(is "$(contains "$(cat "$work/kept.out")" "a passing subtest's log")" false)"
check "the events hold the deepest failure" \
  "$(is "$(jq -c 'select(.Action == "fail" and .Test == "TestBroken/fails/deeper")' "$events/fail.jsonl" | wc -l | tr -d ' ')" 1)"
check "and the passing test of the failed package" "$(is "$(pass_of "$events/fail.jsonl" TestFine)" 1)"
check "each failed test's fail event, its three of them, keeps its elapsed seconds and its time, as a pass and a skip do" \
  "$(is "$(jq -s -r '[.[] | select(.Test and .Action == "fail")] | "\(length) \(map(select(has("Elapsed") and has("Time"))) | length)"' "$events/fail.jsonl")" "3 3")"

echo "case: what a test prints to stdout is where go test prints it, a passing test's and a failed one's"
compare print -count=1 ./print/
check "exits as go test does (1)" "$(is "$kept_status" "$plain_status")"
check "go test prints the passing test's print" "$(contains "$(cat "$work/plain.out")" "^a passing test's own print$")"
check "its stdout is go test's, both prints in their places" "$(same "$work/plain.out" "$work/kept.out")"
compare subprint -count=1 ./subprint/
check "a failed subtest's print is unindented, before its parent's report, as go test prints it" \
  "$(same "$work/plain.out" "$work/kept.out")"

echo "case: a log line longer than test2json's pieces is printed whole, indented once"
compare longlog -count=1 ./longlog/
check "exits 1" "$(is "$kept_status" 1)"
check "its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"

echo "case: a print that reads like a panic is a print"
compare panictext -count=1 ./panictext/
check "exits 1" "$(is "$kept_status" 1)"
check "its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"

echo "case: a test that exits inside itself prints its own prints and none of its log, as go test does"
compare exits -count=1 ./exits/
check "exits as go test does" "$(is "$kept_status" "$plain_status")"
check "its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"
check "the unprinted log line is in the events" "$(contains "$(cat "$events/exits.jsonl")" 'a log line go test never prints')"
compare partialexit -count=1 ./partialexit/
check "a print that ends no line and then an exit: exits as go test does" "$(is "$kept_status" "$plain_status")"
check "its stdout is go test's, the line ended and the log unprinted" "$(same "$work/plain.out" "$work/kept.out")"

echo "case: a print that ends no line, then a t.Log line, which test2json joins into one line"
compare partial -count=1 ./partial/
check "exits 1" "$(is "$kept_status" 1)"
check "its stdout is go test's, the print before the report and the log in it" "$(same "$work/plain.out" "$work/kept.out")"

echo "case: a print that ends in a blank and ends no line, then a t.Log line"
compare spaced -count=1 ./spaced/
check "exits 1" "$(is "$kept_status" 1)"
check "its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"
compare spacedexit -count=1 ./spacedexit/
check "and before an exit, its stdout is go test's, the log unprinted" "$(same "$work/plain.out" "$work/kept.out")"

echo "case: a passing test's t.Log under a file name with a space or a //line label stays out of a failed package's log"
compare labels -count=1 ./labels/
check "exits 1" "$(is "$kept_status" 1)"
check "its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"
check "which prints neither log" "$(is "$(contains "$(cat "$work/kept.out")" "a passing test's log")" false)"

echo "case: the one line -json cannot tell apart, a print laid out as t.Log lays out a line, moves and never adds a line"
compare logshape -count=1 ./logshape/
check "exits 1" "$(is "$kept_status" 1)"
check "the failed test's print is in its report" \
  "$(contains "$(tr '\n' '|' <"$work/kept.out")" '^--- FAIL: TestLogShaped ([0-9.]*s)|    pretend.go:123: ')"
check "the passing test's is not printed" "$(is "$(contains "$(cat "$work/kept.out")" 'pretend.go:124')" false)"
check "the log holds no line go test's does not" \
  "$(is "$(comm -13 <(norm <"$work/plain.out" | sort) <(norm <"$work/kept.out" | sort) | wc -l | tr -d ' ')" 0)"
check "and both prints are in the events" \
  "$(is "$(grep -c 'pretend.go:12[34]' "$events/logshape.jsonl")" 2)"

echo "case: -fuzz prints its progress as go test does, a fuzz test that passes and one that fails"
compare fuzz -count=1 -parallel=1 -run '^FuzzPasses$' -fuzz '^FuzzPasses$' -fuzztime=1x ./fuzz/
check "a passing fuzz test exits 0" "$(is "$kept_status" 0)"
check "go test prints the fuzz progress" "$(contains "$(cat "$work/plain.out")" '^fuzz: elapsed: ')"
check "its stdout is go test's, the progress included" "$(same "$work/plain.out" "$work/kept.out")"
compare fuzzfail -count=1 -parallel=1 -run '^FuzzFails$' -fuzz '^FuzzFails$' -fuzztime=1x ./fuzzfail/
check "a failing fuzz test exits as go test does (1)" "$(is "$kept_status" "$plain_status")"
check "its stdout is go test's, its report in order and no last FAIL line" "$(same "$work/plain.out" "$work/kept.out")"

echo "case: with -v, as a flag, as -test.v or in GOFLAGS, the log is go test -v's"
compare verbose -count=1 -v ./pass/ ./fail/ ./leak/
check "exits as go test does (1)" "$(is "$kept_status" "$plain_status")"
check "its stdout is go test -v's" "$(same "$work/plain.out" "$work/kept.out")"
check "holds the === NAME line -json leaves out" "$(contains "$(cat "$work/kept.out")" '^=== NAME  TestBroken$')"
compare testv -count=1 -test.v ./pass/
check "-test.v prints -v's log" "$(same "$work/plain.out" "$work/kept.out")"
plain GOFLAGS=-v -- -count=1 ./pass/
kept GOFLAGS=-v TEST_EVENTS_FILE="$events/goflags.jsonl" -- go test -count=1 ./pass/
check "GOFLAGS=-v prints -v's log" "$(same "$work/plain.out" "$work/kept.out")"
check "and -v's log is the one printed" "$(contains "$(cat "$work/kept.out")" '^=== RUN   TestPlain$')"
compare vone -count=1 -v=1 ./pass/
check "-v=1 prints -v's log" "$(same "$work/plain.out" "$work/kept.out")"
compare vlast -count=1 -v -v=false ./pass/
check "-v then -v=false, the last winning, prints the plain log" "$(same "$work/plain.out" "$work/kept.out")"
check "and keeps every test's pass in the events" "$(is "$(pass_of "$events/vlast.jsonl" TestNested/inner)" 1)"
plain GOFLAGS=-v -- -count=1 -v=false ./pass/
kept GOFLAGS=-v TEST_EVENTS_FILE="$events/goflags-override.jsonl" -- go test -count=1 -v=false ./pass/
check "GOFLAGS=-v overridden by the command line's -v=false prints the plain log" "$(same "$work/plain.out" "$work/kept.out")"
check "and keeps every test's pass in the events" "$(is "$(pass_of "$events/goflags-override.jsonl" TestPlain)" 1)"

echo "case: a GOFLAGS word go reads quoted keeps its quoting"
plain GOFLAGS="-v '-ldflags=-w -s'" -- -count=1 ./pass/
kept GOFLAGS="-v '-ldflags=-w -s'" TEST_EVENTS_FILE="$events/goflags-quoted.jsonl" -- go test -count=1 ./pass/
check "exits as go test does (0)" "$(is "$kept_status" "$plain_status")"
check "its stdout is go test -v's" "$(same "$work/plain.out" "$work/kept.out")"

echo "case: go test's words end where go's do: a -- hands the rest to the binary unread, and a -skip value of -args ends nothing"
compare dashdash-json -count=1 ./pass/ -- -json
check "a -json after -- is the binary's, not this script's to refuse: exits as go test does (0)" "$(is "$kept_status" "$plain_status")"
check "and its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"
check "and it keeps every test's pass" "$(is "$(pass_of "$events/dashdash-json.jsonl" TestPlain)" 1)"
echo "case: a -test.v the step hands the test binary is refused before the run: the binary's flags, not go test's -v, would decide what -json reports"
for handed in "-args -test.v=false" "-args -test.v=true" "-args -test.v" "-args -update -test.v=false" "-- -test.v=false"; do
  name=$(printf '%s' "$handed" | tr -c 'a-z' '-')
  # shellcheck disable=SC2086 # the handed words are words
  kept TEST_EVENTS_FILE="$events/handed$name.jsonl" -- go test -v -count=1 ./pass/ $handed
  check "\`$handed\` exits 2" "$(is "$kept_status" 2)"
  check "and names the word" "$(contains "$(cat "$work/kept.err")" "hands the test binary its own \`-\{0,1\}-test.v")"
  check "and runs no test" "$([ ! -s "$events/handed$name.jsonl" ] && echo true || echo false)"
done
compare handed-run -count=1 ./pass/ -args -test.run=TestPlain
check "control: a binary flag that is no -test.v runs, as go test runs it" "$(is "$kept_status" "$plain_status")"
check "with go test's log" "$(same "$work/plain.out" "$work/kept.out")"
check "and keeps the test it ran" "$(is "$(pass_of "$events/handed-run.jsonl" TestPlain)" 1)"

echo "case: a -skip pattern of -args ends nothing"
compare skip-args -count=1 -skip '-args' -v ./pass/
check "a -skip pattern of -args is the pattern's: the -v after it prints -v's log" "$(same "$work/plain.out" "$work/kept.out")"
check "which holds the test's own lines too" "$(contains "$(cat "$work/kept.out")" '^=== RUN   TestPlain$')"

echo "case: -bench in GOFLAGS and -list print what go test prints"
plain GOFLAGS='-bench=BenchmarkA -benchtime=1x' -- -count=1 -run '^$' ./bench/
kept GOFLAGS='-bench=BenchmarkA -benchtime=1x' TEST_EVENTS_FILE="$events/bench.jsonl" -- go test -count=1 -run '^$' ./bench/
check "GOFLAGS's -bench streams the benchmark's report" "$(same "$work/plain.out" "$work/kept.out")"
check "which go test prints" "$(contains "$(cat "$work/plain.out")" '^BenchmarkA')"
compare list -count=1 -list 'Test' ./bench/ ./pass/
check "-list prints the names it lists" "$(same "$work/plain.out" "$work/kept.out")"
check "which go test prints" "$(contains "$(cat "$work/plain.out")" '^TestNested$')"

echo "case: go -C <directory> test runs where go test -C runs"
set +e
(cd "$module" && go -C pass test -count=1 .) >"$work/plain.out" 2>"$work/plain.err"
plain_status=$?
set -e
kept TEST_EVENTS_FILE="$events/go-c.jsonl" -- go -C pass test -count=1 .
check "exits as go -C does (0)" "$(is "$kept_status" "$plain_status")"
check "its stdout is go -C's" "$(same "$work/plain.out" "$work/kept.out")"
check "and keeps the package's tests" "$(is "$(pass_of "$events/go-c.jsonl" TestPlain)" 1)"

echo "case: with no package named, go test prints the binary's output as it comes, PASS among it"
plain -- -C pass -count=1
kept TEST_EVENTS_FILE="$events/local.jsonl" -- -C pass go test -count=1
check "exits 0" "$(is "$kept_status" 0)"
check "its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"
check "which holds PASS" "$(contains "$(cat "$work/kept.out")" '^PASS$')"

echo "case: no test the -run names prints go test's [no tests to run]"
compare none -count=1 -run '^TestNone$' ./pass/
check "exits 0" "$(is "$kept_status" 0)"
check "its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"

echo "case: a timed-out binary prints its goroutine dump, and the run fails as go test's does"
kept TEST_EVENTS_FILE="$events/hang.jsonl" -- go test -count=1 -timeout 2s ./hang/
check "exits 1" "$(is "$kept_status" 1)"
check "the dump's first line is printed" "$(contains "$(cat "$work/kept.out")" '^panic: test timed out after 2s$')"
check "it names the test that was running" "$(contains "$(cat "$work/kept.out")" 'TestHang (')"
check "the package's FAIL line and go test's last FAIL end it" \
  "$(contains "$(tail -n 2 "$work/kept.out" | tr '\n' '|')" $'^FAIL\texample.com/events/hang\t[0-9.]*s|FAIL|$')"
check "the test that passed before it is kept" "$(is "$(pass_of "$events/hang.jsonl" TestQuick)" 1)"

echo "case: a package that does not build prints go test's lines, its build output on stderr"
compare broken -count=1 ./broken/ ./pass/
check "exits as go test does (1)" "$(is "$kept_status" "$plain_status")"
check "its stdout is go test's" "$(same "$work/plain.out" "$work/kept.out")"
check "its stderr is go test's" "$(same "$work/plain.err" "$work/kept.err")"
check "the events hold the build failure" \
  "$(is "$(jq -c 'select(.Action == "build-fail")' "$events/broken.jsonl" | wc -l | tr -d ' ')" 1)"

echo "case: a step without an id of its own is refused by name"
kept GITHUB_ACTION=__run_3 -- go test -count=1 ./pass/
check "exits 2" "$(is "$kept_status" 2)"
check "names the id GitHub made up" "$(contains "$(cat "$work/kept.err")" 'this step.s id is `__run_3`')"
check "runs no test" "$([ ! -e "$events/__run_3.jsonl" ] && echo true || echo false)"
kept GITHUB_ACTION= RUNNER_TEMP= -- go test -count=1 ./pass/
check "outside CI, with no TEST_EVENTS_FILE, exits 2" "$(is "$kept_status" 2)"
check "and says what to set" "$(contains "$(cat "$work/kept.err")" 'set TEST_EVENTS_FILE')"
kept TEST_EVENTS_FILE="$events/json.jsonl" -- go test -json -count=1 ./pass/
check "a step passing -json itself exits 2" "$(is "$kept_status" 2)"
kept TEST_EVENTS_FILE="$events/vet.jsonl" -- go vet ./pass/
check "a go command that is not go test exits 2" "$(is "$kept_status" 2)"

echo "case: control: an events file where no event can be kept or read is refused before the run"
kept TEST_EVENTS_FILE="$work/elsewhere/events.jsonl" -- go test -count=1 ./pass/
check "under a runner, a file outside go-test-events/ exits 2" "$(is "$kept_status" 2)"
check "naming the directory the job uploads" "$(contains "$(cat "$work/kept.err")" "outside $events, the directory this job uploads")"
check "and runs no test" "$([ ! -e "$work/elsewhere/events.jsonl" ] && echo true || echo false)"
kept RUNNER_TEMP= TEST_EVENTS_FILE=/dev/null -- go test -count=1 ./pass/
check "/dev/null exits 2" "$(is "$kept_status" 2)"
check "as no regular file" "$(contains "$(cat "$work/kept.err")" 'is not a regular file')"
kept RUNNER_TEMP= TEST_EVENTS_FILE="$work/outside-ci.jsonl" -- go test -count=1 ./pass/
check "outside CI a regular file anywhere is the file" "$(is "$(pass_of "$work/outside-ci.jsonl" TestPlain)" 1)"

echo "case: control: a go that writes no event fails the step, whatever its own status"
stub="$work/stub"
mkdir -p "$stub"
printf '#!/bin/sh\necho "a line that is no event"\nexit "${STUB_STATUS:-0}"\n' >"$stub/go"
chmod +x "$stub/go"
kept PATH="$stub:$PATH" TEST_EVENTS_FILE="$events/empty.jsonl" -- go test -count=1 ./pass/
check "a run that exits 0 with no event exits 1" "$(is "$kept_status" 1)"
check "names the file it wrote nothing to" \
  "$(contains "$(cat "$work/kept.err")" "wrote no test2json event to $events/empty.jsonl (exit 0)")"
check "prints the line that is no event" "$(contains "$(cat "$work/kept.out")" '^a line that is no event$')"
check "and keeps it out of the file" "$(is "$(wc -c <"$events/empty.jsonl" | tr -d ' ')" 0)"
kept PATH="$stub:$PATH" STUB_STATUS=3 TEST_EVENTS_FILE="$events/empty3.jsonl" -- go test -count=1 ./pass/
check "a run that exits 3 with no event keeps its 3" "$(is "$kept_status" 3)"
check "and still says it wrote no event" "$(contains "$(cat "$work/kept.err")" 'wrote no test2json event')"

echo "case: a script that runs go test -json itself keeps its events through TEST_EVENTS_FILE"
appends="$work/appends.sh"
printf '#!/bin/sh\ncd "%s" && go test -json -count=1 ./pass/ >"$1"\ncat "$1" >>"$TEST_EVENTS_FILE"\nexit 4\n' "$module" >"$appends"
chmod +x "$appends"
kept GITHUB_ACTION=count -- "$appends" "$work/report.json"
check "exits as the script does (4)" "$(is "$kept_status" 4)"
check "the script's events are in the step's file" "$(is "$(pass_of "$events/count.jsonl" TestNested/inner)" 1)"
check "prints nothing of its own" "$(is "$(cat "$work/kept.err")" "")"
printf '#!/bin/sh\necho "the script ran and kept its report"\n' >"$work/silent.sh"
chmod +x "$work/silent.sh"
kept TEST_EVENTS_FILE="$events/silent.jsonl" -- "$work/silent.sh"
check "control: a script that appends no event exits 1" "$(is "$kept_status" 1)"
check "and is named the same way" "$(contains "$(cat "$work/kept.err")" "wrote no test2json event to $events/silent.jsonl")"

echo "case: a script that runs go test through this script, under it, appends to the outer file"
printf '#!/bin/sh\ncd "%s" && exec "%s" go test -count=1 ./leak/\n' "$module" "$writer" >"$work/nested.sh"
chmod +x "$work/nested.sh"
kept TEST_EVENTS_FILE="$events/nested.jsonl" -- "$work/nested.sh"
check "exits 0" "$(is "$kept_status" 0)"
check "its log is go test's ok line" "$(contains "$(cat "$work/kept.out")" $'^ok  \texample.com/events/leak\t')"
check "the inner run's events are in the outer file" "$(is "$(pass_of "$events/nested.jsonl" TestLeak)" 1)"

# backend-count.sh as the envoy-go step runs it, its go a stand-in that writes two passes and then
# waits as a long test would, and the step cancelled while it waits: the counter appends each event
# as go writes it, so the passes are kept although the counter never reaches its count. The
# control is a copy of the counter that appends its report only once go test has returned, the
# shape this replaced, which keeps nothing of the same run.
echo "case: a counter script cut short by SIGTERM keeps the events of the tests that ran"
cancel_stub="$work/cancel-stub"
mkdir -p "$cancel_stub" "$work/after/scripts"
cat >"$cancel_stub/go" <<'SH'
#!/bin/sh
for t in TestProbe/aws TestProbe/gcp; do
  printf '{"Action":"pass","Package":"example.com/probe","Test":"%s"}\n' "$t"
done
: >"$PROBE_READY"
exec sleep 30
SH
chmod +x "$cancel_stub/go"
counter="$repo/packages/envoy/scripts/backend-count.sh"
after="$work/after/scripts/backend-count.sh"
# The shape this replaced: the report appended after go test returns, nothing teed.
sed 's#| tee -a "${TEST_EVENTS_FILE:-/dev/null}" >"$events" || go_test_exit=$?#>"$events" || go_test_exit=$?\n[ -z "${TEST_EVENTS_FILE:-}" ] || cat "$events" >>"$TEST_EVENTS_FILE"#' "$counter" >"$after"
chmod +x "$after"
check "the control's copy appends after go test returns, and tees nothing" \
  "$( { grep -q 'cat "$events" >>"$TEST_EVENTS_FILE"' "$after" && ! grep -q 'tee -a' "$after"; } && echo true || echo false)"
# cut_short <name> <counter>: the counter under this script, sent SIGTERM once the stand-in has
# written its two passes; the passes kept in <name>.jsonl, and the step's status in $cut_status.
cut_short() {
  local ready="$work/$1.ready"
  set +e
  env RUNNER_TEMP="$runner" TEST_EVENTS_FILE="$events/$1.jsonl" PATH="$cancel_stub:$PATH" PROBE_READY="$ready" \
    setsid "$writer" "$2" ./cmd/cipher/ -- TestProbe >"$work/$1.out" 2>"$work/$1.err" &
  local pid=$!
  for _ in $(seq 300); do [ -e "$ready" ] && break; sleep 0.1; done
  sleep 0.5
  kill -TERM "$pid"
  wait "$pid"
  cut_status=$?
  kill -KILL -- "-$pid" 2>/dev/null
  set -e
  kept_passes="$(pass_of "$events/$1.jsonl" TestProbe/aws)$(pass_of "$events/$1.jsonl" TestProbe/gcp)"
}
cut_short probe "$counter"
check "cut short, the step exits as its signal says (143)" "$(is "$cut_status" 143)"
check "and keeps both passes the stand-in wrote" "$(is "$kept_passes" 11)"
cut_short probe-after "$after"
check "control: the counter that appends after go test returns keeps neither" "$(is "$kept_passes" 00)"

# A write to the events file that fails: the stand-in writes 200 tests' events, far past a file
# size limit of 4 KiB, which fails a write past it with EFBIG as a full disk fails one with ENOSPC
# (Python ignores SIGXFSZ, so the write fails rather than the process); then it prints a line,
# waits a second as a running test would, marks that it has ended, and exits with STUB_STATUS. The
# file holds an earlier go test's event of the same step, which must outlive the cut.
echo "case: a write to the events file that fails, as on a full disk, stops the keeping and not the run"
disk_stub="$work/disk-stub"
mkdir -p "$disk_stub"
cat >"$disk_stub/go" <<'SH'
#!/bin/sh
i=0
while [ "$i" -lt 200 ]; do
  printf '{"Action":"run","Package":"example.com/disk","Test":"TestDisk%d"}\n' "$i"
  printf '{"Action":"pass","Package":"example.com/disk","Test":"TestDisk%d","Elapsed":0}\n' "$i"
  i=$((i + 1))
done
echo "a line go test prints after the write fails"
sleep 1
: >"$STUB_DONE"
exit "${STUB_STATUS:-0}"
SH
chmod +x "$disk_stub/go"
earlier='{"Action":"pass","Package":"example.com/earlier","Test":"TestEarlier","Elapsed":0}'
# full <name> <status>: the writer over the stand-in, with the file limit; its stdout, stderr and
# status, and in $waited whether the stand-in had ended when the writer returned.
full() {
  printf '%s\n' "$earlier" >"$events/$1.jsonl"
  set +e
  (ulimit -f 4 && cd "$module" && exec env RUNNER_TEMP="$runner" PATH="$disk_stub:$PATH" TEST_EVENTS_FILE="$events/$1.jsonl" \
    STUB_DONE="$work/$1.done" STUB_STATUS="$2" "$writer" go test -count=1 ./pass/) >"$work/kept.out" 2>"$work/kept.err"
  kept_status=$?
  set -e
  waited=$([ -e "$work/$1.done" ] && echo true || echo false)
}
full disk 3
check "go test's own status decides the step (3)" "$(is "$kept_status" 3)"
check "the writer returned only once go test had ended" "$(is "$waited" true)"
check "and printed the line go test printed after the failed write" "$(contains "$(cat "$work/kept.out")" '^a line go test prints after the write fails$')"
check "names the file and the error" \
  "$(contains "$(cat "$work/kept.err")" "writing this run's events to $events/disk.jsonl failed (\[Errno 27\] File too large)")"
check "and cuts the file back to the earlier go test's event, so it holds no part of this run" "$(is "$(cat "$events/disk.jsonl")" "$earlier")"
full disk-passed 0
check "a run whose go test passed fails all the same, its record lost (1)" "$(is "$kept_status" 1)"
check "and its file holds no pass of it" "$(is "$(pass_of "$events/disk-passed.jsonl" TestDisk0)" 0)"

# A fault of the writer's own rendering: an output event whose Output is a number, which the log's
# renderer cannot print, between two tests' events; the stand-in then ends a second later.
echo "case: a fault of the log's rendering stops the log and not the run: every event is kept, go test waited for, and the step fails naming it"
fault_stub="$work/fault-stub"
mkdir -p "$fault_stub"
cat >"$fault_stub/go" <<'SH'
#!/bin/sh
printf '%s\n' '{"Action":"start","Package":"example.com/fault"}' \
  '{"Action":"run","Package":"example.com/fault","Test":"TestBefore"}' \
  '{"Action":"output","Package":"example.com/fault","Test":"TestBefore","Output":5}' \
  '{"Action":"pass","Package":"example.com/fault","Test":"TestBefore","Elapsed":0}' \
  '{"Action":"run","Package":"example.com/fault","Test":"TestAfter"}' \
  '{"Action":"pass","Package":"example.com/fault","Test":"TestAfter","Elapsed":0}' \
  '{"Action":"pass","Package":"example.com/fault","Elapsed":0}'
sleep 1
: >"$STUB_DONE"
SH
chmod +x "$fault_stub/go"
kept PATH="$fault_stub:$PATH" TEST_EVENTS_FILE="$events/fault.jsonl" STUB_DONE="$work/fault.done" -- go test -count=1 ./pass/
check "the step fails though go test passed (1)" "$(is "$kept_status" 1)"
check "the writer returned only once go test had ended" "$([ -e "$work/fault.done" ] && echo true || echo false)"
check "every event is kept, those after the fault among them" \
  "$(is "$(pass_of "$events/fault.jsonl" TestBefore)$(pass_of "$events/fault.jsonl" TestAfter)" 11)"
check "and names the fault" "$(contains "$(cat "$work/kept.err")" 'the log could not be printed past this point (TypeError: ')"

# A go test step cancelled: the stand-in writes one test's pass and a second test's start, then
# waits as that test would; on SIGTERM it writes one more event, marks that it has ended and exits
# 143. The control is a copy of the writer that does not hand go test the signal.
echo "case: a go test step cancelled by SIGTERM: the writer hands go test the signal and keeps every event it wrote, the last after the signal"
term_stub="$work/term-stub"
mkdir -p "$term_stub" "$work/unforwarded"
cat >"$term_stub/go" <<'SH'
#!/bin/sh
trap 'echo "{\"Action\":\"output\",\"Package\":\"example.com/term\",\"Test\":\"TestRunning\",\"Output\":\"signalled\"}"; : >"$STUB_DONE"; exit 143' TERM
printf '%s\n' '{"Action":"run","Package":"example.com/term","Test":"TestDone"}' \
  '{"Action":"pass","Package":"example.com/term","Test":"TestDone","Elapsed":0}' \
  '{"Action":"run","Package":"example.com/term","Test":"TestRunning"}'
: >"$STUB_READY"
# The sleep holds no end of go test's stdout, which the writer reads to its end.
sleep 30 >/dev/null &
wait $!
SH
chmod +x "$term_stub/go"
python3 -c 'import sys, pathlib; s = pathlib.Path(sys.argv[1]).read_text(); old = "    forward_signals(child)\n    # A write that fails"; assert s.count(old) == 1; pathlib.Path(sys.argv[2]).write_text(s.replace(old, "    # A write that fails"))' \
  "$writer" "$work/unforwarded/go_test_events.py"
# cancel <name> <writer>: the writer over the stand-in, sent SIGTERM once the stand-in waits; its
# status in $cancel_status and, in $waited, whether the stand-in had ended when the writer returned.
cancel() {
  set +e
  env RUNNER_TEMP="$runner" TEST_EVENTS_FILE="$events/$1.jsonl" PATH="$term_stub:$PATH" STUB_READY="$work/$1.ready" \
    STUB_DONE="$work/$1.done" setsid python3 "$2" go test -count=1 ./pass/ >"$work/$1.out" 2>"$work/$1.err" &
  local pid=$!
  for _ in $(seq 300); do [ -e "$work/$1.ready" ] && break; sleep 0.1; done
  sleep 0.5
  kill -TERM "$pid"
  wait "$pid"
  cancel_status=$?
  waited=$([ -e "$work/$1.done" ] && echo true || echo false)
  kill -KILL -- "-$pid" 2>/dev/null
  set -e
}
cancel term "$writer"
check "the step exits as its signal says (143)" "$(is "$cancel_status" 143)"
check "the writer returned only once go test had handled the signal and ended" "$(is "$waited" true)"
check "the pass of the test that ended before the signal is kept" "$(is "$(pass_of "$events/term.jsonl" TestDone)" 1)"
check "and the test the signal cut short has its start and the line it wrote after the signal, and no pass or fail" \
  "$(is "$(jq -r 'select(.Test == "TestRunning") | .Action' "$events/term.jsonl" | paste -sd, -)" 'run,output')"
cancel term-unforwarded "$work/unforwarded/go_test_events.py"
check "control: a writer that does not hand go test the signal returns before go test has ended" "$(is "$waited" false)"
check "and keeps no line go test wrote after it" "$(is "$(jq -r 'select(.Test == "TestRunning") | .Action' "$events/term-unforwarded.jsonl" | paste -sd, -)" 'run')"

# A run ended by a kill rather than a signal the writer can pass on, as a runner that kills the
# step's process tree ends it: the writer and go test die at once. What the writer had read is in
# the file all the same: it writes each event unbuffered as it reads it. The control is a copy of
# the writer whose events file is buffered, which loses them.
echo "case: a go test step killed with its process tree keeps every event the writer had read before the kill"
mkdir -p "$work/buffered"
python3 -c 'import sys, pathlib; s = pathlib.Path(sys.argv[1]).read_text(); old = "open(path, \"ab\", buffering=0)"; assert s.count(old) == 1; pathlib.Path(sys.argv[2]).write_text(s.replace(old, "open(path, \"ab\")"))' \
  "$writer" "$work/buffered/go_test_events.py"
# killed <name> <writer>: the writer over the stand-in, its whole process group sent SIGKILL once
# the stand-in has written its events and waits.
killed() {
  set +e
  env RUNNER_TEMP="$runner" TEST_EVENTS_FILE="$events/$1.jsonl" PATH="$term_stub:$PATH" STUB_READY="$work/$1.ready" \
    STUB_DONE="$work/$1.done" setsid python3 "$2" go test -count=1 ./pass/ >"$work/$1.out" 2>"$work/$1.err" &
  local pid=$!
  for _ in $(seq 300); do [ -e "$work/$1.ready" ] && break; sleep 0.1; done
  sleep 0.5
  kill -KILL -- "-$pid"
  wait "$pid"
  killed_status=$?
  set -e
}
killed killed "$writer"
check "the step ends as SIGKILL ends it (137)" "$(is "$killed_status" 137)"
check "the pass the stand-in wrote before the kill is kept" "$(is "$(pass_of "$events/killed.jsonl" TestDone)" 1)"
check "and the start of the test the kill cut short, with no pass or fail" \
  "$(is "$(jq -r 'select(.Test == "TestRunning") | .Action' "$events/killed.jsonl" | paste -sd, -)" 'run')"
killed killed-buffered "$work/buffered/go_test_events.py"
check "control: a writer whose events file is buffered keeps none of them" "$(is "$(wc -c <"$events/killed-buffered.jsonl" | tr -d ' ')" 0)"

# The second form leaves the writing to the script, whose own line tees each event into the file:
# a write that fails there is tee's, so the events written before it stay in the file, and the step
# fails, the script running its pipeline under pipefail. A file size limit fails a write the way a
# full disk does, with an error, only where SIGXFSZ is ignored; the writer hands its command the
# default action (subprocess's restore_signals), so a shell between the writer and the counter
# ignores it again, and the counter's tee inherits that.
echo "case: in the second form a write to the events file that fails leaves part of the run in the file, and the step fails"
tee_stub="$work/tee-stub"
mkdir -p "$tee_stub"
cat >"$tee_stub/go" <<'SH'
#!/bin/sh
i=0
while [ "$i" -lt 200 ]; do
  printf '{"Action":"pass","Package":"example.com/probe","Test":"TestProbe/aws","Elapsed":0}\n'
  i=$((i + 1))
done
SH
chmod +x "$tee_stub/go"
set +e
(ulimit -f 4 && exec env RUNNER_TEMP="$runner" TEST_EVENTS_FILE="$events/tee-full.jsonl" PATH="$tee_stub:$PATH" \
  "$writer" bash -c 'trap "" XFSZ; exec "$0" "$@"' "$counter" ./cmd/cipher/ -- TestProbe) >"$work/tee-full.out" 2>"$work/tee-full.err"
tee_status=$?
set -e
check "the step fails" "$([ "$tee_status" -ne 0 ] && echo true || echo false)"
check "tee names the write that failed" "$(contains "$(cat "$work/tee-full.err")" 'tee: .*tee-full.jsonl: File too large')"
check "and the file keeps the part of the run written before it" \
  "$(kept=$(grep -c '"Action":"pass"' "$events/tee-full.jsonl"); [ "$kept" -gt 0 ] && [ "$kept" -lt 200 ] && echo true || echo false)"

summary "go_test_events.py"
