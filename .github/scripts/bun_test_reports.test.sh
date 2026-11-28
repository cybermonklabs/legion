#!/usr/bin/env bash
# Tests for the record of a bun test run: bun_test_record.py's rule, bun_test_run.py, the wrapper
# that writes the record, and bun_test_reports.py, the reader of a run's artifacts.
#
# The cases follow the rule's five clauses: for each, the run that keeps it reads whole, and each
# way a run can break it (the instances bun 1.4.4 was seen or probed to give) reads as an unknown
# line naming that clause, and, where bun exited 0, fails the step. Every record is written by the
# real wrapper running the real bun (the one setup-bun installed) on scratch test files, but for
# the shapes of the rule's clauses bun does not give on demand, which a stand-in bun writes; the
# job list and the artifacts are a store directory read with --offline, or a stand-in gh for the
# reader's own REST reads.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test-lib.sh
source "$script_dir/test-lib.sh"
reader="$script_dir/bun_test_reports.py"
wrapper="$script_dir/bun_test_run.py"
# The Python that runs both, by its own path, for the cases whose PATH holds no bun or no gh.
python=$(python3 -c 'import sys; print(sys.executable)')

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# The runner the wrapper writes under: its workspace holds the scratch packages, and its temp
# directory the records, as a job's do.
ws="$work/ws"
export GITHUB_WORKSPACE="$ws"
mkdir -p "$work/empty"

# wrap <runner temp> <package> <name> <command...>: the wrapper in $ws/packages/<package>, as a
# step runs it there, with WRAP_PATH as its PATH when that is set; sets wrapped (its exit status)
# and wrap_log (what it printed).
wrap() {
  local temp=$1 package=$2
  shift 2
  set +e
  wrap_log=$(cd "$ws/packages/$package" && PATH="${WRAP_PATH:-$PATH}" RUNNER_TEMP="$temp" "$python" "$wrapper" "$@" 2>&1)
  wrapped=$?
  set -e
}
# package <name> <file> <text>: a scratch package's test file.
package() { mkdir -p "$ws/packages/$1/src" && printf '%s\n' "$3" > "$ws/packages/$1/src/$2"; }
imports='import { afterAll, beforeAll, describe, expect, test } from "bun:test";'

# store <dir> [<runner temp>]: a store whose one job uploaded the records the wrapper wrote under
# <runner temp> (job 101, attempt 1; by default the outcomes run), one job with no bun test
# (102), and the jobs.json the reader keeps.
store() {
  local dir=$1 temp=${2:-$work/t-outcomes}
  mkdir -p "$dir"
  cp -r "$temp/bun-test-reports" "$dir/bun-test-reports-101"
  cat > "$dir/jobs.json" <<'EOF'
[{"id": 101, "name": "pkg / pkg", "conclusion": "failure", "attempt": 1, "steps": [{"name": "Test pkg", "conclusion": "failure"}, {"name": "Keep the bun test reports", "conclusion": "success"}]},
 {"id": 102, "name": "lint / lint", "conclusion": "success", "attempt": 1, "steps": [{"name": "Lint", "conclusion": "success"}]}]
EOF
}

# two_job_store <dir>: two jobs with whole reports, before a case changes the first job's record.
two_job_store() {
  local dir=$1
  store "$dir" "$work/t-ok"
  cp -r "$dir/bun-test-reports-101" "$dir/bun-test-reports-102"
  "$python" - "$dir/jobs.json" <<'EOF'
import json, sys
from pathlib import Path
jobs = Path(sys.argv[1])
first, _ = json.loads(jobs.read_text())
jobs.write_text(json.dumps([first, {**first, "id": 102, "name": "ok / ok"}]))
EOF
}

read_store() { # read_store <dir>: sets out and status
  set +e
  out=$("$python" "$reader" --store "$1" --offline 2>&1)
  status=$?
  set -e
}

row() { # row <name>: the reader's row for that test, as one compact JSON line
  python3 -c '
import json, sys
for line in sys.stdin:
    if line.startswith("{"):
        row = json.loads(line)
        if row.get("name") == sys.argv[1]:
            print(json.dumps({k: row[k] for k in ("job_id", "attempt", "report", "file", "status", "line")}, sort_keys=True))
' "$1" <<<"$out"
}

unknowns() { grep -c '"unknown"' <<<"$out" || true; }
# exit_of <report>: the wrapper's .exit beside it.
exit_of() { cat "$1.exit" 2>/dev/null || echo none; }

# A scratch package with one test of each outcome, two of them under nested describe blocks.
package pkg outcomes.test.ts "$imports
describe(\"outer\", () => {
  describe(\"inner\", () => {
    test(\"passes\", () => { expect(1).toBe(1); });
  });
  test.skip(\"is skipped\", () => {});
});
test(\"fails\", () => { expect(1).toBe(2); });
test.todo(\"is to do\");
test(\"sleeps a little\", async () => { await Bun.sleep(50); });"
wrap "$work/t-outcomes" pkg test bun test
outcomes_status=$wrapped
outcomes_log=$wrap_log
written="$work/t-outcomes/bun-test-reports/packages/pkg/test.xml"

# A package whose one file passes: the honest run each instance below is read beside.
package ok ok.test.ts "$imports
test(\"passes\", () => {});"
wrap "$work/t-ok" ok test bun test
ok_status=$wrapped
honest="$work/t-ok/bun-test-reports/packages/ok/test.xml"
# beside <temp> <name>: runs the passing file again under <temp>, as another run of the same job,
# so the job's artifact holds the honest record beside the instance's.
beside() { WRAP_PATH="" wrap "$1" ok "ok-$2" bun test; }
# clean_beside <name>: the honest run beside the instance reads as its one pass row and no unknown
# line of its own.
clean_beside() {
  check "the honest run beside it still reads clean" \
    "$(is "$(grep -c "\"report\": \"packages/ok/ok-$1.xml\", .*\"status\": \"pass\"" <<<"$out"):$(grep -c "\"report\": \"packages/ok/ok-$1.xml\", \"unknown\"" <<<"$out" || true)" 1:0)"
}

echo "case: the wrapper runs bun, keeps its status and console, and writes the record around it"
check "keeps bun's exit status, 1 for the failing test" "$(is "$outcomes_status" 1)"
check "prints bun's own console" "$(contains "$outcomes_log" '(fail) fails')"
check "and bun's summary, which it reads" "$(contains "$outcomes_log" '^Ran 5 tests across 1 file')"
check "wrote the report" "$([ -s "$written" ] && echo true || echo false)"
check "wrote the marker, holding the command" "$(is "$(cat "$written.run")" 'bun test')"
check "wrote bun's exit status and its count of errors" "$(is "$(exit_of "$written")" '{"status": 1, "errors": 0}')"
check "wrote no pattern for a run without -t" "$([ -e "$written.t" ] && echo false || echo true)"

echo "case: a whole run's tests are rows, with status, file from the root, time, line and attempt"
store "$work/s1"
read_store "$work/s1"
check "exits 0" "$(is "$status" 0)"
check "five rows, all of job 101" "$(is "$(grep -c '"job_id": 101' <<<"$out")" 5)"
check "no row of the job that ran no bun test" "$(is "$(grep -c '"job_id": 102' <<<"$out" || true)" 0)"
check "a nested test's name holds its describe blocks" \
  "$(is "$(row 'outer > inner > passes')" '{"attempt": 1, "file": "packages/pkg/src/outcomes.test.ts", "job_id": 101, "line": 4, "report": "packages/pkg/test.xml", "status": "pass"}')"
check "a failure is a fail" "$(contains "$(row fails)" '"status": "fail"')"
check "a skip is a skip" "$(contains "$(row 'outer > is skipped')" '"status": "skip"')"
check "a todo is a todo" "$(contains "$(row 'is to do')" '"status": "todo"')"
check "test rows follow the report's depth-first order" \
  "$(python3 -c '
import json, sys, xml.etree.ElementTree as ET
expected = [c.get("name") for c in ET.parse(sys.argv[1]).iter("testcase")]
actual = [json.loads(line)["name"].split(" > ")[-1] for line in sys.stdin if line.startswith("{")]
print("true" if actual == expected else "false")' "$written" <<<"$out")"
check "a test's seconds are the time bun wrote for it" \
  "$(python3 -c '
import json, sys, xml.etree.ElementTree as ET
time = [c.get("time") for c in ET.parse(sys.argv[1]).iter("testcase") if c.get("name") == "sleeps a little"]
rows = [json.loads(l) for l in sys.stdin if l.startswith("{")]
seconds = [r["seconds"] for r in rows if r["name"] == "sleeps a little"]
print("true" if len(time) == 1 and float(time[0]) >= 0.04 and seconds == [float(time[0])] else "false")' "$written" <<<"$out")"

echo "case: the honest run of a passing file is whole"
check "the wrapper exits 0" "$(is "$ok_status" 0)"
store "$work/s-ok" "$work/t-ok"
read_store "$work/s-ok"
check "exits 0" "$(is "$status" 0)"
check "one pass row and no unknown line" "$(is "$(grep -c '"status": "pass"' <<<"$out"):$(unknowns)" 1:0)"

# A test that takes long enough to be stopped in.
package slow slow.test.ts "$imports
test(\"slow\", async () => { await Bun.sleep(1500); });"
# until_file <path>: waits up to 30 s for it to exist.
until_file() { for _ in $(seq 300); do [ -e "$1" ] && return 0; sleep 0.1; done; return 1; }
# start_slow <runner temp> [setsid|step]: the wrapper on the slow file in the background: as is, in
# a session of its own (setsid), or as the last command of a step script bash runs, as the runner
# runs a step's `run:`; sets launched, and checks the run's marker is there while bun runs.
start_slow() {
  local temp=$1 seen=false
  printf 'cd %q\n%q %q test bun test\n' "$ws/packages/slow" "$python" "$wrapper" > "$temp.step.sh"
  case ${2:-} in
    setsid) (cd "$ws/packages/slow" && RUNNER_TEMP="$temp" exec setsid "$python" "$wrapper" test bun test > "$temp.log" 2>&1) & ;;
    step) (RUNNER_TEMP="$temp" exec bash -e "$temp.step.sh" > "$temp.log" 2>&1) & ;;
    *) (cd "$ws/packages/slow" && RUNNER_TEMP="$temp" exec "$python" "$wrapper" test bun test > "$temp.log" 2>&1) & ;;
  esac
  launched=$!
  until_file "$temp/bun-test-reports/packages/slow/test.xml.run" && kill -0 "$launched" 2>/dev/null && seen=true
  check "the wrapper wrote the run's marker while bun ran" "$seen"
  sleep 0.3
}
slow_report() { printf '%s' "$1/bun-test-reports/packages/slow/test.xml"; }

echo "case: clause 1 (an exit status): a step killed with bun, which leaves the marker alone"
temp="$work/t-killed"
start_slow "$temp" setsid
kill -KILL -- "-$launched" 2>/dev/null || true
wait "$launched" || true
check "left the marker alone" \
  "$(is "$(cd "$temp/bun-test-reports/packages/slow" && find . -type f | sort | tr '\n' ' ')" './test.xml.run ')"
store "$work/s-killed" "$temp"
read_store "$work/s-killed"
check "the reader exits 1" "$(is "$status" 1)"
check "names clause 1" \
  "$(contains "$out" '"report": "packages/slow/test.xml", "unknown": "clause 1: it has no exit status: bun had not ended, or the wrapper was killed, when the job uploaded its reports')"
check "and clause 2, as it has no report" "$(contains "$out" '"report": "packages/slow/test.xml", "unknown": "clause 2: it has no report"')"

echo "case: clause 1: a step the runner cancels, which signals the step's shell alone"
temp="$work/t-cancelled"
start_slow "$temp" step
kill -TERM "$launched"
set +e
wait "$launched"
shell_status=$?
set -e
check "the step's shell ended on the TERM" "$(is "$shell_status" 143)"
store "$work/s-cancelled" "$temp"
read_store "$work/s-cancelled"
check "an upload while the wrapper runs on finds no exit status: clause 1" \
  "$(is "$status:$(contains "$out" '"unknown": "clause 1: it has no exit status')" 1:true)"
until_file "$temp/bun-test-reports/packages/slow/test.xml.exit"
check "the wrapper, which the TERM did not reach, wrote bun's status once bun ended" \
  "$(is "$(exit_of "$(slow_report "$temp")")" '{"status": 0, "errors": 0}')"
store "$work/s-cancelled-later" "$temp"
read_store "$work/s-cancelled-later"
check "an upload after bun ended on its own reads the run whole" "$(is "$status:$(unknowns)" 0:0)"

echo "case: clause 1: a wrapper killed while bun goes on to write its report"
temp="$work/t-orphan"
start_slow "$temp"
kill -KILL "$launched" 2>/dev/null || true
wait "$launched" || true
until_file "$(slow_report "$temp")" || true
sleep 0.5
check "bun wrote its report and nothing wrote an exit status" \
  "$([ -s "$(slow_report "$temp")" ] && [ ! -e "$(slow_report "$temp").exit" ] && echo true || echo false)"
store "$work/s-orphan" "$temp"
read_store "$work/s-orphan"
check "the reader exits 1, naming clause 1" "$(is "$status:$(contains "$out" '"unknown": "clause 1: it has no exit status')" 1:true)"
check "and prints the test bun reported beside it" "$(contains "$(row slow)" '"status": "pass"')"

echo "case: clause 1: an exit status file that is not the wrapper's"
store "$work/s-exit-text" "$work/t-ok"
echo "zero" > "$work/s-exit-text/bun-test-reports-101/packages/ok/test.xml.exit"
read_store "$work/s-exit-text"
check "exits 1, naming clause 1" \
  "$(is "$status:$(contains "$out" "\"unknown\": \"clause 1: its exit status file test.xml.exit holds 'zero', not the wrapper's\"")" 1:true)"

echo "case: clause 1: an exit status file with deeply nested JSON does not stop the reader"
deep_exit_store="$work/s-deep-exit"
two_job_store "$deep_exit_store"
"$python" -c 'print("[" * 200_000, end="")' \
  >"$deep_exit_store/bun-test-reports-101/packages/ok/test.xml.exit"
read_store "$deep_exit_store"
check "deep JSON: exits 1, naming clause 1 with no traceback" \
  "$(is "$status:$(contains "$out" "\"job_id\": 101, .*\"unknown\": \"clause 1: its exit status file test.xml.exit holds .*not the wrapper's\""):$(contains "$out" Traceback)" 1:true:false)"
check "the reader still prints tests from both jobs" \
  "$(is "$(contains "$out" '"job_id": 101, .*"status": "pass"'):$(contains "$out" '"job_id": 102, .*"status": "pass"')" true:true)"

echo "case: clause 1: an exit status file that is not UTF-8 does not stop the reader"
binary_exit_store="$work/s-binary-exit"
cp -r "$deep_exit_store" "$binary_exit_store"
printf '\xff\xfe' > "$binary_exit_store/bun-test-reports-101/packages/ok/test.xml.exit"
read_store "$binary_exit_store"
check "non-UTF-8 exit: exits 1, naming clause 1 with no traceback" \
  "$(is "$status:$(contains "$out" '"job_id": 101, .*"unknown": "clause 1: its exit status file test.xml.exit cannot be read:'):$(contains "$out" Traceback)" 1:true:false)"
check "after a non-UTF-8 exit status, the reader still prints the next job's test" \
  "$(contains "$out" '"job_id": 102, .*"status": "pass"')"

echo "case: a signal sent to the wrapper itself is passed to bun, and bun's status is still written"
temp="$work/t-term"
start_slow "$temp"
kill -TERM "$launched" 2>/dev/null || true
set +e
wait "$launched"
termed=$?
set -e
check "the wrapper exits with bun's status, 143 for its TERM" "$(is "$termed" 143)"
check "and wrote it" "$(contains "$(exit_of "$(slow_report "$temp")")" '"status": 143')"
store "$work/s-term" "$temp"
read_store "$work/s-term"
check "the reader exits 1, naming clause 2: bun wrote no report" \
  "$(is "$status:$(contains "$out" '"unknown": "clause 2: it has no report: bun exited 143 and wrote none')" 1:true)"

# A stand-in bun, for the shapes of a record bun does not give on demand: it writes STANDIN_REPORT
# (a file) as its report, prints STANDIN_STDERR on its standard error, and exits STANDIN_EXIT.
mkdir -p "$work/standin"
cat > "$work/standin/bun" <<'EOF'
#!/usr/bin/env bash
for word in "$@"; do case $word in --reporter-outfile=*) out=${word#*=} ;; esac; done
if [ -n "${STANDIN_REPORT:-}" ]; then cp "$STANDIN_REPORT" "$out"; fi
printf '%b' "${STANDIN_STDERR:-}" >&2
if [ "${STANDIN_LATE_STDERR:-}" = yes ]; then
  # The child waits for the wrapper to reap bun, writes once, then exits and closes the pipe.
  python3 -c '
import os, sys, time
parent = int(sys.argv[1])
while True:
    try:
        os.kill(parent, 0)
    except ProcessLookupError:
        break
    time.sleep(0.01)
sys.stderr.write("x" * 99_999 + "\n")
' "$$" >/dev/null &
fi
exit "${STANDIN_EXIT:-0}"
EOF
chmod +x "$work/standin/bun"
summary_ok=' 1 pass\n 0 fail\nRan 1 test across 1 file. [1.00ms]\n'
printf '<testsuites name="bun test" tests="1"><testsuite name="src/ok.test.ts" file="src/ok.test.ts">' > "$work/cut.xml"
# standin <temp> <name> <report|""> <stderr> <exit>: the wrapper on the stand-in bun.
standin() {
  STANDIN_REPORT=$3 STANDIN_STDERR=$4 STANDIN_EXIT=$5 WRAP_PATH="$work/standin:$PATH" wrap "$1" ok "$2" bun test
}

echo "case: a process a test leaves holding bun's standard error keeps the wrapper at most DRAIN_SECONDS"
package holder holder.test.ts "$imports
test(\"leaves a child\", async () => {
  const child = Bun.spawn([\"sleep\", \"25\"], { stdout: \"ignore\", stderr: \"inherit\" });
  await Bun.write(process.env.HOLDER_PID_FILE!, String(child.pid));
  child.unref();
});"
started=$(date +%s)
HOLDER_PID_FILE="$work/holder.pid" wrap "$work/t-holder" holder test bun test
held=$(( $(date +%s) - started ))
kill "$(cat "$work/holder.pid")" 2>/dev/null || true
check "the wrapper ended within DRAIN_SECONDS (5) and a margin, not with the child ($held s)" \
  "$(is "$wrapped:$(( held < 15 ))" 0:1)"
check "and wrote bun's status, its summary read" \
  "$(is "$(exit_of "$work/t-holder/bun-test-reports/packages/holder/test.xml")" '{"status": 0, "errors": 0}')"

echo "case: a child writes past the summary buffer's limit after bun exits"
STANDIN_LATE_STDERR=yes standin "$work/t-late-stderr" test "$honest" "$summary_ok" 0
check "the wrapper exits 0 despite the child's output" "$(is "$wrapped" 0)"
check "and wrote bun's status and summary before the child could replace it" \
  "$(is "$(exit_of "$work/t-late-stderr/bun-test-reports/packages/ok/test.xml")" '{"status": 0, "errors": 0}')"
check "the step still receives all 100,000 bytes of the child's stderr" \
  "$(if "$python" -c 'import sys; sys.exit("x" * 99_999 not in sys.stdin.read().splitlines())' <<<"$wrap_log"; then
       echo true
     else
       echo false
     fi)"

echo "case: clause 2 (a report bun 1.4.4 writes): bun exits 0 and leaves a report that does not parse"
standin "$work/t-cut" test "$work/cut.xml" "$summary_ok" 0
check "the wrapper fails the step" "$(is "$wrapped" 1)"
check "naming clause 2" "$(contains "$wrap_log" '^::error::bun exited 0 and its run .*/packages/ok/test.xml breaks clause 2: the report does not parse')"
beside "$work/t-cut" cut
store "$work/s-cut" "$work/t-cut"
read_store "$work/s-cut"
check "the reader exits 1, naming clause 2" \
  "$(is "$status:$(contains "$out" '"report": "packages/ok/test.xml", "unknown": "clause 2: the report does not parse')" 1:true)"
clean_beside cut

echo "case: clause 2: an outfile bun cannot write (bun exits 0 and writes nothing)"
mkdir -p "$work/t-unwritable/bun-test-reports/packages/ok/test.xml"
wrap "$work/t-unwritable" ok test bun test
check "bun itself exited 0" "$(contains "$(exit_of "$work/t-unwritable/bun-test-reports/packages/ok/test.xml")" '"status": 0')"
check "the wrapper fails the step, naming clause 2" \
  "$(is "$wrapped:$(contains "$wrap_log" 'breaks clause 2: it has no report: bun exited 0 and wrote none')" 1:true)"
beside "$work/t-unwritable" unwritable
store "$work/s-unwritable" "$work/t-unwritable"
read_store "$work/s-unwritable"
check "the reader exits 1, naming clause 2" \
  "$(is "$status:$(contains "$out" '"report": "packages/ok/test.xml", "unknown": "clause 2: it has no report: bun exited 0 and wrote none')" 1:true)"
clean_beside unwritable

echo "case: clause 2: a test that calls process.exit (bun exits 0, no report, no summary)"
package exits e.test.ts "$imports
test(\"exits\", () => { process.exit(0); });"
wrap "$work/t-exit" exits test bun test
check "bun itself exited 0" "$(contains "$(exit_of "$work/t-exit/bun-test-reports/packages/exits/test.xml")" '"status": 0')"
check "the wrapper fails the step, naming clauses 2 and 3" \
  "$(is "$wrapped:$(contains "$wrap_log" 'breaks clause 2: it has no report'):$(contains "$wrap_log" 'breaks clause 3: bun printed no summary')" 1:true:true)"
beside "$work/t-exit" exit
store "$work/s-exit" "$work/t-exit"
read_store "$work/s-exit"
check "the reader exits 1, naming clause 2" \
  "$(is "$status:$(contains "$out" '"report": "packages/exits/test.xml", "unknown": "clause 2: it has no report: bun exited 0 and wrote none')" 1:true)"
clean_beside exit

echo "case: clause 2: a report cut short, or whose test count is not its number of entries"
store "$work/s4"
head -c 400 "$written" > "$work/s4/bun-test-reports-101/packages/pkg/test.xml"
read_store "$work/s4"
check "cut short: exits 1, naming clause 2" \
  "$(is "$status:$(contains "$out" '"report": "packages/pkg/test.xml", "unknown": "clause 2: the report does not parse')" 1:true)"
store "$work/s5"
python3 - "$work/s5/bun-test-reports-101/packages/pkg/test.xml" <<'EOF'
import sys, xml.etree.ElementTree as ET
tree = ET.parse(sys.argv[1])
for parent in tree.iter():
    for case in list(parent.findall("testcase")):
        if case.get("name") == "fails":
            parent.remove(case)
tree.write(sys.argv[1])
EOF
read_store "$work/s5"
check "a count off by one: exits 1, naming the two counts" \
  "$(is "$status:$(contains "$out" '"unknown": "clause 2: the report says 5 tests and holds 4 entries"')" 1:true)"

echo "case: clause 2: the report's test count is a digit but not a decimal"
store "$work/s-nondecimal-count" "$work/t-ok"
python3 - "$work/s-nondecimal-count/bun-test-reports-101/packages/ok/test.xml" <<'EOF'
import sys, xml.etree.ElementTree as ET
tree = ET.parse(sys.argv[1])
tree.getroot().set("tests", "²")
tree.write(sys.argv[1], encoding="utf-8")
EOF
read_store "$work/s-nondecimal-count"
check "non-decimal count: exits 1, naming clause 2 and the two counts with no traceback" \
  "$(is "$status:$(contains "$out" '"unknown": "clause 2: the report says \\u00b2 tests and holds 1 entries"'):$(contains "$out" Traceback)" 1:true:false)"

echo "case: clause 2: unsupported XML encodings, an oversized count, and non-finite times"
for fault in utf-7 rot13 oversized-count nan inf; do
  dir="$work/s-$fault"
  two_job_store "$dir"
  "$python" - "$dir" "$fault" <<'EOF'
import sys, xml.etree.ElementTree as ET
from pathlib import Path

report, fault = Path(sys.argv[1]) / "bun-test-reports-101/packages/ok/test.xml", sys.argv[2]
tree = ET.parse(report)
if fault in ("utf-7", "rot13"):
    report.write_text(f'<?xml version="1.0" encoding="{fault}"?>\n'
                      + ET.tostring(tree.getroot(), encoding="unicode"))
elif fault == "oversized-count":
    tree.getroot().set("tests", "9" * 5000)
    tree.write(report)
else:
    tree.find(".//testcase").set("time", fault)
    tree.write(report)
EOF
  case $fault in
    oversized-count) reason="the report's test count is longer than 64 characters\"}" ;;
    nan|inf) reason="the report does not parse: its test 'passes' has time '$fault'.*not numbers" ;;
    *) reason="the report does not parse:" ;;
  esac
  set +e
  out=$("$python" "$reader" --store "$dir" --offline 2>"$work/read-errors")
  status=$?
  set -e
  check "$fault: exits 1 with a clause 2 unknown and no traceback" \
    "$(is "$status:$(contains "$out" "\"job_id\": 101, .*\"unknown\": \"clause 2: $reason"):$(contains "$(cat "$work/read-errors")" Traceback)" 1:true:false)"
  check "$fault: the next job of the store still reads as a pass" \
    "$(contains "$out" '^{"job_id": 102, .*"status": "pass"')"
  check "$fault: every output line is strict JSON" \
    "$(if "$python" -c '
import json, sys
def reject_constant(value):
    raise ValueError(value)
try:
    for line in sys.stdin:
        json.loads(line, parse_constant=reject_constant)
except ValueError:
    sys.exit(1)
' <<<"$out"; then echo true; else echo false; fi)"
done

echo "case: clause 2: suite nesting beyond the bound does not stop the reader"
store "$work/s-deep-report" "$work/t-ok"
python3 - "$work/s-deep-report/bun-test-reports-101/packages/ok/test.xml" <<'EOF'
import sys
from pathlib import Path
levels = 100_000
Path(sys.argv[1]).write_text(
    '<testsuites tests="1">' + '<testsuite name="nested">' * levels
    + '<testcase name="passes" file="src/ok.test.ts" line="2" time="0"/>'
    + '</testsuite>' * levels + '</testsuites>')
EOF
read_store "$work/s-deep-report"
check "deep XML: exits 1, naming clause 2 and the suite nesting bound with no traceback" \
  "$(is "$status:$(contains "$out" '"report": "packages/ok/test.xml", "unknown": "clause 2: the report.s <testsuite> nesting exceeds the bound of 100 levels"'):$(contains "$out" Traceback)" 1:true:false)"

echo "case: clause 2: a test with no time, line or file is a format this reader does not know, not a test of 0 s"
for attribute in time line file; do
  store "$work/s-no-$attribute"
  python3 - "$work/s-no-$attribute/bun-test-reports-101/packages/pkg/test.xml" "$attribute" <<'EOF'
import sys, xml.etree.ElementTree as ET
tree = ET.parse(sys.argv[1])
for case in tree.iter("testcase"):
    if case.get("name") == "sleeps a little":
        del case.attrib[sys.argv[2]]
tree.write(sys.argv[1])
EOF
  read_store "$work/s-no-$attribute"
  check "with no $attribute: exits 1, naming the test and the attribute" \
    "$(is "$status:$(contains "$out" "\"unknown\": \"clause 2: the report does not parse: its entry 'sleeps a little' has no $attribute attribute")" 1:true)"
  check "with no $attribute: prints no row of the report" "$(is "$(grep -c '"status"' <<<"$out" || true)" 0)"
done

echo "case: clause 2: a -t pattern beside the report that is not a literal name is refused, never compiled"
# An artifact is written by the run's own tests too: a pattern built to backtrack, against a long
# test name, would hang a reader that compiled it.
package long long.test.ts "$imports
test(\"$(printf 'a%.0s' $(seq 64))!\", () => {});"
wrap "$work/t-long" long test bun test
store "$work/s-bad-t" "$work/t-long"
printf '%s' '(a+)+$' > "$work/s-bad-t/bun-test-reports-101/packages/long/test.xml.t"
started=$(date +%s)
set +e
out=$(timeout 60 "$python" "$reader" --store "$work/s-bad-t" --offline 2>&1)
status=$?
set -e
check "a backtracking pattern: exits 1 at once, naming clause 2 and the special characters" \
  "$(is "$status:$(contains "$out" '"unknown": "clause 2: its -t pattern test.xml.t is not a literal name: it holds \$ ( ) +')":$(( $(date +%s) - started < 10 )) 1:true:1)"
# The one bound counts characters, including when UTF-8 needs four bytes for each one.
python3 -c 'print(chr(0x1f600) * 1001, end="")' > "$work/s-bad-t/bun-test-reports-101/packages/long/test.xml.t"
read_store "$work/s-bad-t"
check "a pattern longer than the bound: exits 1, naming it" \
  "$(is "$status:$(contains "$out" 'clause 2: its -t pattern test.xml.t is not a literal name: it is 1001 characters, above the bound of 1000')" 1:true)"

echo "case: clause 3 (no error outside a test): a file that does not load, in a run with no failing test"
package load a.test.ts "$imports
test(\"loads\", () => {});"
package load b.test.ts "import { x } from \"./missing\";
$imports
test(\"never\", () => { x(); });"
wrap "$work/t-load" load test bun test
check "the step fails with bun's own status" "$(is "$wrapped" 1)"
check "bun's console counts the error, and the wrapper kept it" \
  "$(is "$(contains "$wrap_log" '^ 1 error$'):$(exit_of "$work/t-load/bun-test-reports/packages/load/test.xml")" 'true:{"status": 1, "errors": 1}')"
check "bun's report holds the loaded file's one test and no failure" \
  "$(is "$(python3 -c 'import sys, xml.etree.ElementTree as ET; r = ET.parse(sys.argv[1]).getroot(); print(r.get("tests"), r.get("failures"))' "$work/t-load/bun-test-reports/packages/load/test.xml")" '1 0')"
beside "$work/t-load" load
store "$work/s-load" "$work/t-load"
read_store "$work/s-load"
check "the reader exits 1, naming clause 3 and the error" \
  "$(is "$status:$(contains "$out" '"report": "packages/load/test.xml", "unknown": "clause 3: bun counted 1 error(s) outside any test (a file that did not load')" 1:true)"
check "and clause 3 alone, not clause 5 as well" "$(is "$(grep -c '"report": "packages/load/test.xml", "unknown"' <<<"$out")" 1)"
check "still prints the test that ran" "$(contains "$(row loads)" '"status": "pass"')"
clean_beside load

echo "case: clause 3: a file that does not load, in a run that also has a failing test"
package loadfail a.test.ts "$imports
test(\"loads\", () => {});
test(\"fails here\", () => { expect(1).toBe(2); });"
package loadfail b.test.ts "import { x } from \"./missing\";
$imports
test(\"never\", () => { x(); });"
wrap "$work/t-loadfail" loadfail test bun test
beside "$work/t-loadfail" loadfail
store "$work/s-loadfail" "$work/t-loadfail"
read_store "$work/s-loadfail"
check "the reader exits 1, naming clause 3, though the report holds a failing test" \
  "$(is "$status:$(contains "$out" '"report": "packages/loadfail/test.xml", "unknown": "clause 3: bun counted 1 error(s)')" 1:true)"
check "and prints both tests the report holds" "$(is "$(row 'fails here' | grep -c fail):$(row loads | grep -c pass)" 1:1)"
clean_beside loadfail

echo "case: clause 3: a file that does not load, in a run whose summary also prints the snapshot it added"
# bun 1.4.4 prints `snapshots: +1 added` between ` 1 error` and `Ran N tests across M files.` when
# a run writes a new snapshot (--update-snapshots, or CI=false).
package snapload a.test.ts "$imports
test(\"snaps\", () => { expect({ a: 1 }).toMatchSnapshot(); });
test(\"fails here\", () => { expect(1).toBe(2); });"
package snapload b.test.ts "import { x } from \"./missing\";
$imports
test(\"never\", () => { x(); });"
wrap "$work/t-snapload" snapload test bun test --update-snapshots
check "bun's console counts the error above its snapshots line" \
  "$(is "$(contains "$wrap_log" '^ 1 error$'):$(contains "$wrap_log" '^snapshots: +1 added$')" true:true)"
check "the wrapper kept bun's count of errors" \
  "$(is "$(exit_of "$work/t-snapload/bun-test-reports/packages/snapload/test.xml")" '{"status": 1, "errors": 1}')"
beside "$work/t-snapload" snapload
store "$work/s-snapload" "$work/t-snapload"
read_store "$work/s-snapload"
check "the reader exits 1, naming clause 3, though the report holds a failing test" \
  "$(is "$status:$(contains "$out" '"report": "packages/snapload/test.xml", "unknown": "clause 3: bun counted 1 error(s)')" 1:true)"
clean_beside snapload

echo "case: clause 3: bun exits 0 and prints no summary, or a summary that counts errors"
standin "$work/t-nosummary" test "$honest" "" 0
check "no summary: the wrapper fails the step, naming clause 3" \
  "$(is "$wrapped:$(contains "$wrap_log" 'breaks clause 3: bun printed no summary')" 1:true)"
standin "$work/t-errors" test "$honest" ' 1 pass\n 0 fail\n 2 error\nRan 1 test across 1 file. [1.00ms]\n' 0
check "two errors: the wrapper fails the step, naming clause 3 and the count" \
  "$(is "$wrapped:$(contains "$wrap_log" 'breaks clause 3: bun counted 2 error(s)')" 1:true)"
standin "$work/t-stray" test "$honest" ' 9 error\na line a test printed\n 1 pass\n 0 fail\nRan 1 test across 1 file. [1.00ms]\n' 0
check "an error line above the last summary is not bun's count: the step passes" \
  "$(is "$wrapped:$(exit_of "$work/t-stray/bun-test-reports/packages/ok/test.xml")" '0:{"status": 0, "errors": 0}')"

echo "case: clause 3: bun that cannot start (not on PATH)"
WRAP_PATH="$work/empty" wrap "$work/t-nobun" ok test bun test
check "the wrapper exits 127, naming the error" "$(is "$wrapped:$(contains "$wrap_log" '::error::bun_test_run.py: bun could not start')" 127:true)"
check "and wrote why beside the marker" "$(contains "$(exit_of "$work/t-nobun/bun-test-reports/packages/ok/test.xml")" '"status": 127, "errors": null, "start": ')"
store "$work/s-nobun" "$work/t-nobun"
read_store "$work/s-nobun"
check "the reader exits 1, naming clause 3, not a cancel" \
  "$(is "$status:$(contains "$out" '"unknown": "clause 3: bun could not start (')" 1:true)"
check "and no clause 1" "$(is "$(contains "$out" 'clause 1')" false)"

echo "case: clause 4 (no failure with no line): a beforeAll that throws"
package hook a.test.ts "$imports
describe(\"guarded\", () => {
  beforeAll(() => { throw new Error(\"boom\"); });
  test(\"never runs\", () => {});
});
describe(\"guarded too\", () => {
  beforeAll(() => { throw new Error(\"boom again\"); });
  test(\"never runs either\", () => {});
});
test(\"plain\", () => {});"
wrap "$work/t-hook" hook test bun test
check "the step fails with bun's status" "$(is "$wrapped" 1)"
beside "$work/t-hook" hook
store "$work/s-hook" "$work/t-hook"
read_store "$work/s-hook"
check "the reader exits 1, naming clause 4, the describe and the file" \
  "$(is "$status:$(contains "$out" "\"unknown\": \"clause 4: a beforeAll or afterAll hook of 'guarded' in packages/hook/src/a.test.ts threw: the tests of that describe may not have run, and those that did not are not in the report\"")" 1:true)"
check "two hooks that threw give two clause 4 lines, one for each" \
  "$(is "$(grep -c '"report": "packages/hook/test.xml", "unknown": "clause 4: ' <<<"$out")" 2)"
check "and prints the report's other test beside it" "$(contains "$(row plain)" '"status": "pass"')"
check "and no clause 2 or clause 5" "$(is "$(contains "$out" 'clause [25]')" false)"
clean_beside hook

echo "case: clause 4: an afterAll at the top of a file that throws"
package afterall a.test.ts "$imports
afterAll(() => { throw new Error(\"late\"); });
test(\"ran\", () => {});"
wrap "$work/t-afterall" afterall test bun test
store "$work/s-afterall" "$work/t-afterall"
read_store "$work/s-afterall"
check "the reader exits 1, naming clause 4 at the top of the file" \
  "$(is "$status:$(contains "$out" '"unknown": "clause 4: a beforeAll or afterAll hook at the top in packages/afterall/src/a.test.ts threw: the tests of that file may not have run, and those that did not are not in the report"')" 1:true)"
check "and prints the test that ran, which the report holds" "$(contains "$(row ran)" '"status": "pass"')"

echo "case: clause 4: a --parallel worker that crashes"
package crash a.test.ts "$imports
test(\"a1\", () => {});"
package crash b.test.ts "$imports
test(\"b1 ran\", () => {});
test(\"dies\", () => { process.kill(process.pid, \"SIGKILL\"); });"
wrap "$work/t-crash" crash test bun test --parallel=2
check "the step fails with bun's status" "$(is "$wrapped" 1)"
beside "$work/t-crash" crash
store "$work/s-crash" "$work/t-crash"
read_store "$work/s-crash"
check "the reader exits 1, naming clause 4 and the file the worker ran" \
  "$(is "$status:$(contains "$out" '"unknown": "clause 4: the --parallel worker running packages/crash/src/b.test.ts crashed: the tests of that file it had not reported are not in the report"')" 1:true)"
check "and prints the other file's test" "$(contains "$(row a1)" '"status": "pass"')"
check "and the crashed file's test it had reported" "$(contains "$(row 'b1 ran')" '"status": "pass"')"
clean_beside crash

echo "case: clause 4: the hooks of a skipped describe are entries bun writes with no line, and no test"
package hooks hooks.test.ts "$imports
describe.skipIf(true)(\"skipped with hooks\", () => {
  beforeAll(() => {});
  afterAll(() => {});
  test(\"inner\", () => {});
});
test(\"plain\", () => {});"
wrap "$work/t-hooks" hooks test bun test
hooked="$work/t-hooks/bun-test-reports/packages/hooks/test.xml"
check "the wrapper exits 0" "$(is "$wrapped" 0)"
check "bun wrote two (unnamed) entries with no line, and counted them" \
  "$(is "$(python3 -c '
import sys, xml.etree.ElementTree as ET
root = ET.parse(sys.argv[1]).getroot()
hooks = [c for c in root.iter("testcase") if c.get("name") == "(unnamed)" and c.get("line") is None]
print(len(hooks), root.get("tests"))' "$hooked")" '2 4')"
store "$work/s-hooks" "$work/t-hooks"
read_store "$work/s-hooks"
check "exits 0, printing the two tests and no entry of a hook" \
  "$(is "$status:$(grep -o '"name": "[^"]*"' <<<"$out" | tr '\n' ';')" '0:"name": "skipped with hooks > inner";"name": "plain";')"
# entry <dir> <python>: edits the first (unnamed) entry of the store's hooks report.
entry() {
  store "$1" "$work/t-hooks"
  python3 - "$1/bun-test-reports-101/packages/hooks/test.xml" "$2" <<'EOF'
import sys, xml.etree.ElementTree as ET
tree = ET.parse(sys.argv[1])
case = next(c for c in tree.iter("testcase") if c.get("name") == "(unnamed)")
exec(sys.argv[2])
tree.write(sys.argv[1])
EOF
  read_store "$1"
}
entry "$work/s-hook-line" 'case.set("line", "3"); case.set("time", "0")'
check "a skipped (unnamed) entry with a line is a test, read as a skip" \
  "$(is "$status:$(row 'skipped with hooks > (unnamed)' | grep -c '"status": "skip"')" 0:1)"
entry "$work/s-hook-ran" 'case.remove(case.find("skipped"))'
check "an (unnamed) entry with no line, neither skipped nor failed, breaks clause 2" \
  "$(is "$status:$(contains "$out" "clause 2: the report does not parse: its entry 'skipped with hooks > (unnamed)' has no line")" 1:true)"
entry "$work/s-hook-name" 'case.set("name", "(another)")'
check "a skipped entry with no line of another name breaks clause 2" \
  "$(is "$status:$(contains "$out" "clause 2: the report does not parse: its entry 'skipped with hooks > (another)' has no line")" 1:true)"

echo "case: clause 5 (a non-zero exit the record explains): bun exits non-zero with nothing to show for it"
standin "$work/t-unexplained" test "$honest" "$summary_ok" 3
check "the wrapper exits with bun's status" "$(is "$wrapped" 3)"
store "$work/s-unexplained" "$work/t-unexplained"
read_store "$work/s-unexplained"
check "the reader exits 1, naming clause 5" \
  "$(is "$status:$(contains "$out" '"unknown": "clause 5: bun exited 3, and the record shows no failure that explains it"')" 1:true)"
check "and prints the test the report holds" "$(contains "$(row passes)" '"status": "pass"')"

echo "case: a run given -t keeps its pattern, and a test it did not select is unselected, not skipped"
# bun writes a test its -t pattern leaves out as <skipped />, as it writes a test.skip.
package sel selection.test.ts "$imports
describe(\"group\", () => {
  test(\"picked one\", () => {});
  test.skip(\"picked but skipped\", () => {});
});
test(\"left out\", () => {});"
wrap "$work/t-sel" sel test bun test -t "group picked"
selected="$work/t-sel/bun-test-reports/packages/sel/test.xml"
check "the wrapper exits 0" "$(is "$wrapped" 0)"
check "the wrapper wrote the pattern beside the report" "$(is "$(cat "$selected.t")" 'group picked')"
store "$work/s7" "$work/t-sel"
read_store "$work/s7"
check "exits 0" "$(is "$status" 0)"
check "bun wrote the test.skip and the test -t left out, both as <skipped>" \
  "$(is "$(python3 - "$selected" <<'EOF'
import sys, xml.etree.ElementTree as ET
print(sorted(c.get("name") for c in ET.parse(sys.argv[1]).iter("testcase") if c.find("skipped") is not None))
EOF
)" "['left out', 'picked but skipped']")"
check "the selected test passed" "$(contains "$(row 'group > picked one')" '"status": "pass"')"
check "the selected test.skip is a skip" "$(contains "$(row 'group > picked but skipped')" '"status": "skip"')"
check "the test the pattern left out is unselected" "$(contains "$(row 'left out')" '"status": "unselected"')"
check "the files beside the report are read as no run of their own" "$(is "$(unknowns)" 0)"
rm -f "$work/s7/bun-test-reports-101/packages/sel/test.xml.t"
read_store "$work/s7"
check "with no pattern beside it, the left-out test reads as a skip" "$(contains "$(row 'left out')" '"status": "skip"')"
wrap "$work/t-sel-long" sel test bun test "--test-name-pattern=group picked"
check "the pattern's --test-name-pattern=<p> form is kept too" "$(is "$wrapped:$(cat "$work/t-sel-long/bun-test-reports/packages/sel/test.xml.t")" '0:group picked')"

echo "case: an attached -t pattern is kept on the command line and in a script"
wrap "$work/t-sel-attached" sel cli bun test -tpicked
check "the wrapper writes picked from a command-line -tpicked" \
  "$(is "$wrapped:$(cat "$work/t-sel-attached/bun-test-reports/packages/sel/cli.xml.t" 2>/dev/null || echo none)" 0:picked)"
store "$work/s-sel-attached" "$work/t-sel-attached"
read_store "$work/s-sel-attached"
check "the test left out by the command-line -tpicked is unselected" \
  "$(is "$status:$(contains "$(row 'left out')" '"status": "unselected"')" 0:true)"
printf '%s\n' '{"scripts": {"picked": "bun test -tpicked"}}' > "$ws/packages/sel/package.json"
wrap "$work/t-sel-attached-script" sel test bun run picked
check "the wrapper writes picked from a package script's -tpicked" \
  "$(is "$wrapped:$(cat "$work/t-sel-attached-script/bun-test-reports/packages/sel/test.xml.t" 2>/dev/null || echo none)" 0:picked)"

echo "case: the wrapper refuses, before bun starts, a run whose record could not be whole"
cat > "$ws/packages/ok/package.json" <<'EOF'
{"scripts": {
  "test": "bun test",
  "chain": "bun test src && bun test src",
  "newline": "bun test src\nbun test src",
  "carriage-return": "bun test src\r",
  "build": "tsc",
  "picked": "bun test -t passes",
  "dots": "bun test --reporter=dots"
}}
EOF
# refused <name> <command...>: sets refusal, true when the wrapper exits 2 and wrote no marker.
refused() {
  local name=$1
  shift
  wrap "$work/t-refused" ok "$name" "$@"
  refusal=false
  if [ "$wrapped" = 2 ] && [ ! -e "$work/t-refused/bun-test-reports/packages/ok/$name.xml.run" ]; then refusal=true; fi
}
refused re bun test -t '(?<g>x)'
check "a -t pattern that is not a literal name, which bun would run" "$refusal"
check "and says so, naming its special characters" "$(contains "$wrap_log" "the -t pattern '(?<g>x)' is not a literal name: it holds ( ) ?")"
refused attached-re bun test '-t(a+)+$'
check "an attached pattern with special characters is refused before bun starts" \
  "$(is "$refusal:$(contains "$wrap_log" 'is not a literal name')" true:true)"
refused dots bun run dots
check "a reporter the script gives" "$refusal"
refused twice bun run picked -t other
check "a -t the script gives, and one the command adds" "$refusal"
refused chain bun run chain
check "a bun run of a script that chains two bun tests" "$refusal"
check "and says the reporter would reach the last alone" "$(contains "$wrap_log" "not one \`bun test\` command")"
refused newline bun run newline
check "a bun run of a script that chains two bun tests with a newline is refused before bun starts" \
  "$(is "$refusal:$(contains "$wrap_log" "not one \`bun test\` command")" true:true)"
refused carriage-return bun run carriage-return
check "a bun run of a script with a carriage return is refused before bun starts" \
  "$(is "$refusal:$(contains "$wrap_log" "not one \`bun test\` command")" true:true)"
refused build bun run build
check "a bun run of a script that is no bun test" "$refusal"
refused none bun run none
check "a bun run of a script the package does not have" "$refusal"
set +e
subdir=$(cd "$ws/packages/ok/src" && RUNNER_TEMP="$work/t-refused" "$python" "$wrapper" sub bun run test 2>&1)
subdir_status=$?
set -e
check "a bun run from a directory with no package.json" \
  "$(is "$subdir_status:$(contains "$subdir" 'which holds no package.json')" 2:true)"
refused 'aé' bun test
check "a report name with a letter outside ASCII" "$refusal"
refused other node test
check "a command that is no bun" "$refusal"
refused other bun test --reporter=dots
check "a command with a reporter of its own" "$refusal"
refused other bun test -t a --test-name-pattern=b
check "two -t patterns, in either form" "$refusal"
wrap "$work/t-refused" ok taken bun test
wrap "$work/t-refused" ok taken bun test
check "a name another run of the job took" "$(is "$wrapped:$(contains "$wrap_log" 'already wrote')" 2:true)"
set +e
outside=$(cd "$work" && RUNNER_TEMP="$work/t-refused" "$python" "$wrapper" outside bun test 2>&1)
outside_status=$?
set -e
check "a working directory outside the workspace" \
  "$(is "$outside_status:$(contains "$outside" 'is outside GITHUB_WORKSPACE')" 2:true)"
wrap "$work/t-script" ok test bun run test
check "and runs a bun run of a script that is one bun test" "$(is "$wrapped:$(exit_of "$work/t-script/bun-test-reports/packages/ok/test.xml")" '0:{"status": 0, "errors": 0}')"
wrap "$work/t-script" ok picked bun run picked
check "and keeps the -t pattern a script gives, as the command's" "$(is "$wrapped:$(cat "$work/t-script/bun-test-reports/packages/ok/picked.xml.t")" '0:passes')"

echo "case: a job's record: nothing uploaded, no run, a cancel, a store of another reader, an artifact of no job"
store "$work/s2"
rm -rf "$work/s2/bun-test-reports-101"
read_store "$work/s2"
check "an artifact the store does not hold: exits 1, saying so, not that GitHub has none" \
  "$(is "$status:$(contains "$out" '"job_id": 101.*"unknown": "the store .*/s2 holds no bun-test-reports-101; its upload step concluded success"')" 1:true)"
store "$work/s3"
rm -rf "$work/s3/bun-test-reports-101/packages"
read_store "$work/s3"
check "an artifact that holds no run: exits 1" \
  "$(is "$status:$(contains "$out" '"unknown": "the artifact bun-test-reports-101 holds no run: no report and no run marker"')" 1:true)"
store "$work/s6"
python3 - "$work/s6/jobs.json" <<'EOF'
import json, sys
jobs = json.load(open(sys.argv[1]))
jobs[0]["conclusion"] = "cancelled"
json.dump(jobs, open(sys.argv[1], "w"))
EOF
read_store "$work/s6"
check "a cancelled job: exits 1, saying its later steps did not run" \
  "$(is "$status:$(contains "$out" '"job_id": 101.*"report": null, "unknown": "the job ended cancelled: its bun test steps after the cancel did not run')" 1:true)"
check "and still prints the tests its reports hold" "$(is "$(grep -c '"status"' <<<"$out")" 5)"
mkdir -p "$work/s-nojobs"
read_store "$work/s-nojobs"
check "no jobs.json: exits 1 with one run line, and no traceback" \
  "$(is "$status:$(contains "$out" '^{"run": null, "unknown": "the store .* holds no job list this reader wrote: jobs.json: '):$(contains "$out" Traceback)" 1:true:false)"
store "$work/s-oldjobs"
python3 - "$work/s-oldjobs/jobs.json" <<'EOF'
import json, sys
jobs = json.load(open(sys.argv[1]))
for job in jobs:
    del job["attempt"]
json.dump(jobs, open(sys.argv[1], "w"))
EOF
read_store "$work/s-oldjobs"
check "a job list without attempts: exits 1, naming what it lacks, and prints no row" \
  "$(is "$status:$(contains "$out" 'lacks its id, name, conclusion, attempt or steps'):$(grep -c '"status"' <<<"$out" || true)" 1:true:0)"
store "$work/s-renamed"
python3 - "$work/s-renamed/jobs.json" <<'EOF'
import json, sys
jobs = json.load(open(sys.argv[1]))
jobs[0]["steps"][1]["name"] = "Upload the reports"
json.dump(jobs, open(sys.argv[1], "w"))
EOF
read_store "$work/s-renamed"
check "an artifact of a job with no upload step of that name: exits 1, naming it" \
  "$(is "$status:$(contains "$out" "\"unknown\": \"the artifact bun-test-reports-101 is named for no job of the run with a step named 'Keep the bun test reports'")" 1:true)"
store "$work/s-partial" "$work/t-ok"
mkdir -p "$work/s-partial/bun-test-reports-999.partial"
read_store "$work/s-partial"
check "a download's .partial directory left in the store is no artifact" "$(is "$status:$(unknowns)" 0:0)"

# A stand-in gh on PATH answers the reader's REST reads of run 7 from files, so the reader's own
# job listing is held: GH_ANSWERS names a directory with one file per path it answers (`--slurp`
# wraps the one page in a list, as gh does), and a path it holds no file for fails as GitHub's
# 502 did.
mkdir -p "$work/bin" "$work/answers"
cat > "$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
path=${*: -1}
path=${path#repos/acme/repo/}
file="$GH_ANSWERS/$(printf '%s' "$path" | tr '/?&=' '____').json"
if [ ! -f "$file" ]; then echo "gh: Server Error (HTTP 502)" >&2; exit 1; fi
case " $* " in
  *" --slurp "*) printf '['; cat "$file"; printf ']' ;;
  *) cat "$file" ;;
esac
EOF
chmod +x "$work/bin/gh"
answer_file() { printf '%s' "$work/answers/$(printf '%s' "$1" | tr '/?&=' '____').json"; }
answer() { printf '%s' "$2" > "$(answer_file "$1")"; }
job() { # job <id> <name> <started> <with upload: yes|no>
  local steps='[{"name": "Test", "conclusion": "success"}]'
  [ "$4" = yes ] && steps='[{"name": "Test", "conclusion": "success"}, {"name": "Keep the bun test reports", "conclusion": "success"}]'
  printf '{"id": %s, "name": "%s", "conclusion": "success", "started_at": "%s", "completed_at": "%s", "steps": %s}' "$1" "$2" "$3" "$3" "$steps"
}
answer "actions/runs/7" '{"id": 7, "run_attempt": 2}'
answer "actions/runs/7/attempts/1/jobs?per_page=50" "{\"jobs\": [$(job 101 'pkg / pkg' 2026-11-27T10:00:00Z yes), $(job 102 'go / go' 2026-11-27T10:00:00Z no)]}"
# The second attempt re-ran go / go (a new id, a new time) and lists pkg / pkg again, a copy under
# a new id with its first run's start and end, whose artifact is 101's.
answer "actions/runs/7/attempts/2/jobs?per_page=50" "{\"jobs\": [$(job 201 'pkg / pkg' 2026-11-27T10:00:00Z yes), $(job 202 'go / go' 2026-11-27T11:00:00Z no)]}"
answer "actions/runs/7/artifacts?per_page=100" '{"artifacts": [{"id": 9, "name": "bun-test-reports-101", "expired": false, "size_in_bytes": 4096}]}'
read_run() { # read_run <store dir>: the reader online against the stand-in; sets out and status
  set +e
  out=$(PATH="$work/bin:$PATH" GH_ANSWERS="$work/answers" "$python" "$reader" 7 --repo acme/repo ${1:+--store "$1"} 2>&1)
  status=$?
  set -e
}

echo "case: a later attempt's copy of a job it did not re-run is read as that job, not as a job whose record is missing"
store "$work/s8" "$work/t-ok"
rm -f "$work/s8/jobs.json"
read_run "$work/s8"
check "exits 0" "$(is "$status" 0)"
check "reads job 101's test once, as attempt 1's" "$(is "$(row passes)" '{"attempt": 1, "file": "packages/ok/src/ok.test.ts", "job_id": 101, "line": 2, "report": "packages/ok/test.xml", "status": "pass"}')"
check "prints nothing of the copy 201" "$(is "$(grep -c '"job_id": 201' <<<"$out" || true)" 0)"
check "and no unknown line" "$(is "$(unknowns)" 0)"
check "keeps the attempt in the store's job list" "$(contains "$(cat "$work/s8/jobs.json")" '"id": 101, "name": "pkg / pkg", "conclusion": "success", "attempt": 1')"

echo "case: a job re-run in a later attempt prints each attempt's tests with its attempt"
zipped() { # zipped <dir> <zip>: the artifact GitHub serves for an uploaded directory
  python3 - "$1" "$2" <<'EOF'
import sys, zipfile
from pathlib import Path
with zipfile.ZipFile(sys.argv[2], "w") as z:
    for p in sorted(Path(sys.argv[1]).rglob("*")):
        if p.is_file():
            z.write(p, p.relative_to(sys.argv[1]).as_posix())
EOF
}
answer "actions/runs/7/attempts/2/jobs?per_page=50" "{\"jobs\": [$(job 201 'pkg / pkg' 2026-11-27T11:00:00Z yes), $(job 202 'go / go' 2026-11-27T11:00:00Z no)]}"
answer "actions/runs/7/artifacts?per_page=100" '{"artifacts": [{"id": 9, "name": "bun-test-reports-101", "expired": false, "size_in_bytes": 4096}, {"id": 10, "name": "bun-test-reports-201", "expired": false, "size_in_bytes": 4096}]}'
zipped "$work/t-ok/bun-test-reports" "$(answer_file actions/artifacts/9/zip)"
zipped "$work/t-ok/bun-test-reports" "$(answer_file actions/artifacts/10/zip)"
# A download cut short in an earlier read left its .partial directory in the store.
mkdir -p "$work/s-rerun/bun-test-reports-101.partial/packages"
printf 'stale' > "$work/s-rerun/bun-test-reports-101.partial/packages/stale.xml"
read_run "$work/s-rerun"
check "exits 0, downloading both artifacts" "$(is "$status" 0)"
check "prints attempt 1's row of job 101 and attempt 2's of job 201" \
  "$(is "$(grep -o '"job_id": [0-9]*, "job": "pkg / pkg", "attempt": [0-9]*' <<<"$out" | tr '\n' ';')" '"job_id": 101, "job": "pkg / pkg", "attempt": 1;"job_id": 201, "job": "pkg / pkg", "attempt": 2;')"
check "the stale .partial directory is not unpacked into the artifact" \
  "$([ ! -e "$work/s-rerun/bun-test-reports-101/packages/stale.xml" ] && echo true || echo false)"

echo "case: an artifact that cannot be downloaded is an UNKNOWN line for its job, not a traceback"
rm -f "$(answer_file actions/artifacts/10/zip)"
printf 'not a zip' > "$(answer_file actions/artifacts/9/zip)"
read_run ""
check "exits 1" "$(is "$status" 1)"
check "names job 201's failed download and gh's error" \
  "$(contains "$out" '"job_id": 201, "job": "pkg / pkg", "attempt": 2, "report": null, "unknown": "the artifact bun-test-reports-201 could not be downloaded: gh: Server Error (HTTP 502)"')"
check "names job 101's artifact that does not unpack" \
  "$(contains "$out" '"job_id": 101, .*"unknown": "the artifact bun-test-reports-101 could not be unpacked: File is not a zip file"')"
check "no traceback" "$(is "$(contains "$out" Traceback)" false)"

echo "case: an artifact larger than the bound is refused by name, never downloaded or unpacked in part"
answer "actions/runs/7/attempts/2/jobs?per_page=50" "{\"jobs\": [$(job 201 'pkg / pkg' 2026-11-27T11:00:00Z yes), $(job 202 'go / go' 2026-11-27T11:00:00Z no)]}"
answer "actions/runs/7/artifacts?per_page=100" '{"artifacts": [{"id": 9, "name": "bun-test-reports-101", "expired": false, "size_in_bytes": 67108865}, {"id": 10, "name": "bun-test-reports-201", "expired": false}]}'
read_run ""
check "a listed size above the bound: exits 1, naming it" \
  "$(is "$status:$(contains "$out" '"job_id": 101, .*"unknown": "the artifact bun-test-reports-101 is not downloaded: the run lists it at 67,108,865 bytes, above the bound of 67,108,864 bytes"')" 1:true)"
check "no listed size: refused, naming it" \
  "$(contains "$out" '"job_id": 201, .*"unknown": "the artifact bun-test-reports-201 is not downloaded: the run lists no size for it (None)')"
answer "actions/runs/7/artifacts?per_page=100" '{"artifacts": [{"id": 9, "name": "bun-test-reports-101", "expired": false, "size_in_bytes": 4096}, {"id": 10, "name": "bun-test-reports-201", "expired": false, "size_in_bytes": 4096}]}'
zipped "$work/t-ok/bun-test-reports" "$(answer_file actions/artifacts/10/zip)"
python3 - "$(answer_file actions/artifacts/9/zip)" <<'EOF'
import sys, zipfile
with zipfile.ZipFile(sys.argv[1], "w", zipfile.ZIP_DEFLATED) as z:
    z.writestr("packages/ok/test.xml", b"\0" * (64 * 1024 * 1024 + 1))
EOF
read_store_dir="$work/s-bomb"
read_run "$read_store_dir"
check "members that unpack above the bound: exits 1, naming it, and unpacks nothing" \
  "$(is "$status:$(contains "$out" '"job_id": 101, .*"unknown": "the artifact bun-test-reports-101 is not unpacked: its members unpack to 67,108,865 bytes, above the bound of 67,108,864 bytes"'):$([ -e "$read_store_dir/bun-test-reports-101" ] && echo kept || echo none)" 1:true:none)"
check "and reads the job beside it" "$(contains "$out" '"job_id": 201, .*"status": "pass"')"

echo "case: an artifact with too many zip members is refused by name, before any extraction"
member_zip="$(answer_file actions/artifacts/9/zip)"
python3 - "$member_zip" <<'EOF'
import sys, zipfile
with zipfile.ZipFile(sys.argv[1], "w") as z:
    for i in range(10_001):
        z.writestr(f"empty-{i}", b"")
EOF
answer "actions/runs/7/artifacts?per_page=100" "{\"artifacts\": [{\"id\": 9, \"name\": \"bun-test-reports-101\", \"expired\": false, \"size_in_bytes\": $(stat -c '%s' "$member_zip")}, {\"id\": 10, \"name\": \"bun-test-reports-201\", \"expired\": false, \"size_in_bytes\": 4096}]}"
read_store_dir="$work/s-many-members"
read_run "$read_store_dir"
check "too many members: exits 1, names the artifact and the bound, and unpacks nothing" \
  "$(is "$status:$(contains "$out" '"job_id": 101, .*"unknown": "the artifact bun-test-reports-101 is not unpacked: it holds 10,001 members, above the bound of 10,000"'):$([ ! -e "$read_store_dir/bun-test-reports-101" ] && [ ! -e "$read_store_dir/bun-test-reports-101.partial" ] && echo none || echo unpacked)" 1:true:none)"
check "and reads the job beside the artifact with too many members" \
  "$(contains "$out" '"job_id": 201, .*"status": "pass"')"

echo "case: an artifact the run does not list, or lists expired, is an UNKNOWN line for its job"
answer "actions/runs/7/artifacts?per_page=100" '{"artifacts": [{"id": 10, "name": "bun-test-reports-201", "expired": true, "size_in_bytes": 4096}]}'
read_run ""
check "exits 1" "$(is "$status" 1)"
check "names job 101's missing artifact and its upload step's conclusion" \
  "$(contains "$out" '"job_id": 101, .*"unknown": "the artifact bun-test-reports-101 is missing (the run lists no such artifact); its upload step concluded success"')"
check "names job 201's expired one" \
  "$(contains "$out" '"job_id": 201, .*"unknown": "the artifact bun-test-reports-201 is expired; its upload step concluded success"')"
check "prints no test row" "$(is "$(grep -c '"status"' <<<"$out" || true)" 0)"

echo "case: a job list that cannot be read, or a gh that cannot start, makes the whole run UNKNOWN"
rm -f "$(answer_file "actions/runs/7/attempts/2/jobs?per_page=50")"
read_run ""
check "a 502: exits 1, naming it, with no row" \
  "$(is "$status:$(contains "$out" '"run": "7", "unknown": "the run.s jobs or artifacts could not be read: gh: Server Error (HTTP 502)'):$(grep -c '"status"' <<<"$out" || true)" 1:true:0)"
set +e
out=$(PATH="$work/empty" "$python" "$reader" 7 --repo acme/repo 2>&1)
status=$?
set -e
check "no gh on PATH: exits 1 with the run line, and no traceback" \
  "$(is "$status:$(contains "$out" '"run": "7", "unknown": "the run.s jobs or artifacts could not be read: gh could not start: '):$(contains "$out" Traceback)" 1:true:false)"

summary "bun_test_record.py, bun_test_run.py and bun_test_reports.py"
