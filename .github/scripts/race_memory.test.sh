#!/usr/bin/env bash
# Tests for `race_memory.py`: a command's status passes through, a peak over the limit fails the
# step and names the tests that were running, a run within the limit ends with the peak, a
# `go test -json` stream's output reaches the log, and a line names at most NAMES_BYTES of tests.
#
# Each case runs the real script around a stand-in command — a shell or a Python program that
# prints a -json stream and grows — so no Go, no race binary and no database are needed. The two
# functions the sampling loop reads its numbers with, `summarize` and `choose_peak`, and the
# `decision_line` written from what they return, are called on their own over fixture
# `/proc/<pid>/status` texts written here, because one of the rule's two branches cannot be
# reached on a kernel that reports no `VmHWM` (gVisor) and the other cannot be reached on one
# that does.
#
# Run from anywhere: .github/scripts/race_memory.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
sampler="$script_dir/race_memory.py"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# run_sampler <limit in kB, or '' for the default> <command...>: runs the script under test and
# leaves its merged output in $out and its status in $status.
run_sampler() {
  local limit=$1
  shift
  set +e
  if [ -z "$limit" ]; then
    out=$(env -u RACE_MEM_LIMIT_KB "$sampler" "$@" 2>&1)
  else
    out=$(env "RACE_MEM_LIMIT_KB=$limit" "$sampler" "$@" 2>&1)
  fi
  status=$?
  set -e
}

# peak <python expression over race_memory>: the peak functions called with no process involved.
peak() {
  env PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 -c "$1"
}

echo "case: a failing command's status passes through"
run_sampler '' sh -c 'exit 3'
check "exits 3" "$(is "$status" 3)"
check "names the default limit" "$(contains "$out" 'the limit is 8388608 kB (the default')"

echo "case: a peak over the limit fails the run and names the test that was running"
# Prints a -json stream naming a test as running, a parallel test that ran, paused and was let go
# on, and another left paused, then grows past a 1 kB limit and lives until the case has seen the
# sampler take its second sample (the first at 0 s, the next 5 s later, by when the stream has
# been read), and only then ends: the case waits on the sampler's own lines, not on a clock, so a
# sampler a loaded runner holds back still samples the grown process. The 60 s bounds only end a
# run whose sample never comes, which the checks then report.
release=$(mktemp -u)
grower='
import json, os, sys, time
for event in [
    {"Action": "run", "Test": "TestTheSamplerNamesMe"},
    {"Action": "run", "Test": "TestAParallelOneLetGoOn"},
    {"Action": "pause", "Test": "TestAParallelOneLetGoOn"},
    {"Action": "cont", "Test": "TestAParallelOneLetGoOn"},
    {"Action": "run", "Test": "TestAParallelOneStillPaused"},
    {"Action": "output", "Test": "TestAParallelOneStillPaused", "Output": "=== PAUSE TestAParallelOneStillPaused\n"},
    {"Action": "pause", "Test": "TestAParallelOneStillPaused"},
]:
    sys.stdout.write(json.dumps(event) + "\n")
sys.stdout.flush()
block = b"x" * (64 * 1024 * 1024)
deadline = time.monotonic() + 60
while not os.path.exists(sys.argv[1]) and time.monotonic() < deadline:
    time.sleep(0.05)
sys.stdout.write(str(len(block)) + "\n")
'
sampled=$(mktemp)
env "RACE_MEM_LIMIT_KB=1" "$sampler" python3 -c "$grower" "$release" > "$sampled" 2>&1 &
sampler_pid=$!
for _ in $(seq 1200); do
  [ "$(grep -c -- ' s, VmRSS ' "$sampled" || true)" -ge 2 ] && break
  sleep 0.05
done
touch "$release"
set +e
wait "$sampler_pid"
status=$?
set -e
out=$(cat "$sampled")
rm -f "$release" "$sampled"
decision=$(grep -- 'had these tests running' <<<"$out" || true)
check "exits 1" "$(is "$status" 1)"
check "names the limit it was given" "$(contains "$out" 'the limit is 1 kB (RACE_MEM_LIMIT_KB)')"
check "its decision line names the tests at the highest sample" \
  "$(contains "$decision" 'had these tests running: TestAParallelOneLetGoOn, TestTheSamplerNamesMe')"
check "its decision line leaves out a test that is paused at its t.Parallel" \
  "$(is "$(contains "$decision" 'TestAParallelOneStillPaused')" false)"
check "the paused test's own output still reaches the log" \
  "$(contains "$out" '^=== PAUSE TestAParallelOneStillPaused$')"
check "says the peak passed the limit" "$(contains "$out" 'passed the limit of 1 kB')"
# The grower holds a 64 MiB block in a Python process of a few tens of MB, so its peak is at least
# 65,536 kB and far under 512 MiB, whichever field it is.
peak_seen=$(sed -n 's/.*the peak of \([0-9][0-9]*\) kB passed.*/\1/p' <<<"$out")
check "its peak is the grown process's, the 64 MiB it holds and not far more" \
  "$([ "${peak_seen:-0}" -ge 65536 ] && [ "${peak_seen:-0}" -le 524288 ] && echo true || echo false)"
# How that line ends depends on the kernel, so this is the end-to-end check only — that the
# field the sampler chose reaches the line main writes. The rule itself, both of its branches,
# is pinned by the two fixture cases below, on either kernel.
if [ "$(contains "$out" 'the peak was [0-9][0-9]* kB (VmHWM),')" = true ]; then
  check "warns that a VmHWM peak may be between two samples" \
    "$(contains "$out" 'VmHWM can be reached between two samples')"
else
  check "leaves the between-samples warning off a VmRSS peak" \
    "$(is "$(contains "$out" 'may belong to another moment')" false)"
fi
check "took a sample at 0 s and another at 5 s" \
  "$([ "$(contains "$out" '^race memory: at 0\.[0-9] s, VmRSS ')" = true ] &&
    [ "$(contains "$out" '^race memory: at \([5-9]\|[1-9][0-9]\)\.[0-9] s, VmRSS ')" = true ] && echo true || echo false)"

echo "case: a run within the limit exits 0 and ends with the peak"
run_sampler '' sh -c 'echo the command spoke'
check "exits 0" "$(is "$status" 0)"
check "passes the command's own output through" "$(contains "$out" '^the command spoke$')"
check "its last line gives the peak and the limit" \
  "$(contains "${out##*$'\n'}" '^race memory: the peak was [0-9][0-9]* kB (Vm.*), the limit 8388608 kB$')"

echo "case: the peak is the largest VmHWM when the kernel gives one"
hwm=$(peak '
import race_memory
walk = race_memory.summarize([
    "Name:\tgo\nPPid:\t1\nVmRSS:\t    1000 kB\nVmHWM:\t    4200 kB\n",
    "Name:\tdocs.test\nPPid:\t2\nVmRSS:\t    3000 kB\nVmHWM:\t    3900 kB\n",
], 0.0, "none")
peak_kb, field = race_memory.choose_peak(walk.hwm_kb, walk.rss_kb)
print("%d %s" % (peak_kb, field))
print(race_memory.decision_line(peak_kb, field, 1, walk))
')
check "takes the largest VmHWM, not the largest VmRSS" "$(contains "$hwm" '^4200 VmHWM$')"
check "its decision line warns the peak may belong to another moment" \
  "$(contains "$hwm" 'VmHWM can be reached between two samples, so the peak may belong')"

echo "case: the peak falls back to the highest sampled VmRSS where there is no VmHWM"
rss=$(peak '
import race_memory
walk = race_memory.summarize([
    "Name:\tgo\nPPid:\t1\nVmRSS:\t    1000 kB\n",
    "Name:\tdocs.test\nPPid:\t2\nVmRSS:\t    3000 kB\n",
], 0.0, "none")
peak_kb, field = race_memory.choose_peak(walk.hwm_kb, walk.rss_kb)
print("%d %s" % (peak_kb, field))
print(race_memory.decision_line(peak_kb, field, 1, walk))
')
check "takes the highest sampled VmRSS" "$(contains "$rss" '^3000 VmRSS$')"
check "its decision line gives the peak and the highest sample it came from" \
  "$(contains "$rss" 'the peak of 3000 kB passed the limit of 1 kB; the highest sampled VmRSS, 3000 kB')"
check "its decision line leaves the between-samples warning off a VmRSS peak" \
  "$(is "$(contains "$rss" 'may belong to another moment')" false)"

echo "case: a -json stream's output reaches the log"
stream='
import json, sys
for event in [
    {"Action": "run", "Test": "TestA"},
    {"Action": "output", "Test": "TestA", "Output": "--- FAIL: TestA (0.00s)\n"},
    {"Action": "fail", "Test": "TestA"},
    {"Action": "output", "Output": "FAIL\tlegion/internal/dispatch/docs\t0.10s\n"},
]:
    sys.stdout.write(json.dumps(event) + "\n")
'
run_sampler '' python3 -c "$stream"
check "exits 0" "$(is "$status" 0)"
check "the --- FAIL line shows" "$(contains "$out" '^--- FAIL: TestA (0.00s)$')"
check "the package's own line shows" "$(contains "$out" '^FAIL.*legion/internal/dispatch/docs')"
check "the -json envelope does not" "$(is "$(contains "$out" '"Action"')" false)"

echo "case: a line names at most NAMES_BYTES of tests and counts the rest"
# 3,000 tests running at once, each name about as long as a generated table's subtest: main's
# sampler, which named them all, wrote a line of about 190 kB, which `gh run view --log` (gh up to
# 2.105) stops reading at and prints nothing after (job 140239891619).
capped=$(peak '
import race_memory
tests = race_memory.RunningTests()
for n in range(3000):
    tests.record({"Action": "run", "Test": "TestATableOfEveryCombination/%04d-N-E-P-L-N-E-P-L" % n})
line = race_memory.Sample(seconds=1.0, rss_kb=1, hwm_kb=1, comm="docs.test", tests=tests.names()).line()
print(len(line.encode()))
print(line)
print(race_memory.named(["TestA", "TestB"]))
print(race_memory.named([]))
print(race_memory.named(["T" * (race_memory.NAMES_BYTES + 1), "TestB"]))
# The budget is bytes, separators charged: with names of two-byte letters, a count of characters
# or a separator left uncharged names more than the budget holds.
wide = sorted("TestÄÖÜ/%04d-ÄÖÜ-ÄÖÜ-ÄÖÜ-ÄÖÜ" % n for n in range(3000))
text = race_memory.named(wide)
names = text[: text.index(", and ")]
shown = names.split(", ")
used = len(names.encode())
following = len(wide[len(shown)].encode()) + 2
print("budget %s" % ("held" if used <= race_memory.NAMES_BYTES < used + following else "broken %d %d" % (used, following)))
')
# 49-byte names: 40 of them with their 39 separators are 2,038 bytes, a 41st would make 2,089.
check "it names the first tests in order" \
  "$(contains "$capped" 'tests running: TestATableOfEveryCombination/0000-N-E-P-L-N-E-P-L, TestATableOfEveryCombination/0001-')"
check "it names as many as the budget holds and counts the rest exactly" \
  "$(contains "$capped" 'TestATableOfEveryCombination/0039-N-E-P-L-N-E-P-L, and 2960 more (3000 in all)$')"
check "the line stays far under 64 KiB" \
  "$([ "${capped%%$'\n'*}" -le 2300 ] && echo true || echo false)"
check "the budget is bytes with the separators, and the next name would pass it" \
  "$(contains "$capped" '^budget held$')"
check "a list under the cap is named whole" "$(contains "$capped" '^TestA, TestB$')"
check "no test running is none" "$(contains "$capped" '^none$')"
check "a first name over the cap is counted, not cut" \
  "$(contains "$capped" '^2 running; the first name alone passes the line.s 2048-byte cap$')"

echo "case: a sample after output that ends mid-line starts a line of its own"
# A -json Output fragment can end mid-line (a test's print with no newline): the stream sends it
# whole, then holds the rest of its line until the case has seen the sampler's second sample, so
# a sample written straight after the fragment would be glued to it, one line as long as both.
# (Plain output cannot do this: the script reads the command's stdout a line at a time.)
release=$(mktemp -u)
fragment='
import json, os, sys, time
def event(**fields):
    sys.stdout.write(json.dumps(fields) + "\n")
    sys.stdout.flush()
event(Action="run", Test="TestAPrintWithNoNewline")
event(Action="output", Test="TestAPrintWithNoNewline", Output="partial output with no newline")
deadline = time.monotonic() + 60
while not os.path.exists(sys.argv[1]) and time.monotonic() < deadline:
    time.sleep(0.05)
event(Action="output", Test="TestAPrintWithNoNewline", Output="\n")
event(Action="pass", Test="TestAPrintWithNoNewline")
'
sampled=$(mktemp)
env -u RACE_MEM_LIMIT_KB "$sampler" python3 -c "$fragment" "$release" > "$sampled" 2>&1 &
sampler_pid=$!
for _ in $(seq 1200); do
  [ "$(grep -c -- ' s, VmRSS ' "$sampled" || true)" -ge 2 ] && break
  sleep 0.05
done
touch "$release"
set +e
wait "$sampler_pid"
status=$?
set -e
out=$(cat "$sampled")
rm -f "$release" "$sampled"
check "exits 0" "$(is "$status" 0)"
check "the fragment keeps its own line" "$(contains "$out" '^partial output with no newline$')"
check "the sample after it starts its own line" \
  "$(contains "$(grep -A1 -- '^partial output with no newline$' <<<"$out" | tail -n 1)" '^race memory: at [0-9.]* s, VmRSS ')"
check "no sample is glued to the fragment" "$(is "$(contains "$out" 'newline.*race memory')" false)"

echo "case: a process on the machine whose name is no UTF-8 does not end the walk"
# The walk reads every process's status, and a process names itself with any bytes
# (prctl PR_SET_NAME); this one stays up while the sampler takes its first sample.
named_dir=$(mktemp -d)
python3 -c '
import ctypes, sys, time
ctypes.CDLL(None).prctl(15, b"bad\xff\xfename", 0, 0, 0)
open(sys.argv[1] + "/ready", "w").close()
time.sleep(30)
' "$named_dir" &
bad_pid=$!
for _ in $(seq 200); do [ -e "$named_dir/ready" ] && break; sleep 0.05; done
# The control: the process is up, and the kernel gives its name as bytes that are no UTF-8.
bad_name=$(python3 -c '
import sys
print(b"Name:\tbad\xff\xfename" in open("/proc/%s/status" % sys.argv[1], "rb").read())
' "$bad_pid" 2>/dev/null || echo False)
run_sampler '' sh -c 'exit 0'
kill "$bad_pid" 2>/dev/null || true
wait "$bad_pid" 2>/dev/null || true
rm -rf "$named_dir"
check "a process whose name is no UTF-8 was up for the walk" "$(is "$bad_name" True)"
check "exits 0" "$(is "$status" 0)"
check "writes no decode error" "$(is "$(contains "$out" 'UnicodeDecodeError')" false)"
check "still ends with the peak" "$(contains "$out" '^race memory: the peak was [0-9][0-9]* kB')"

summary "race_memory.py"
