#!/usr/bin/env bash
# Tests for `race_memory.py`: a command's status passes through, a peak over the limit fails the
# step and names the tests that were running, a run within the limit ends with the peak, a
# `go test -json` stream's output reaches the log, a line names at most NAMES_BYTES of tests, the
# sampler reads the files of its command's own processes alone, and neither a process's name nor a
# byte of the command's stdout that is not UTF-8 stops a run before it gives its result.
#
# Each case runs the real script around a stand-in command — a shell or a Python program that
# prints a -json stream and grows — so no Go, no race binary and no database are needed. The two
# functions the sampling loop reads its numbers with, `summarize` and `choose_peak`, and the
# `decision_line` written from what they return, are called on their own over fixture
# `/proc/<pid>/status` texts written here, because one of the rule's two branches cannot be
# reached on a kernel that reports no `VmHWM` (gVisor) and the other cannot be reached on one
# that does. The name cases name the command's own process, which its samples read, and each
# first checks that the kernel shows the name's bytes as they were written: a kernel that escaped
# them would leave the case nothing to test, and it fails rather than passes.
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

# drive_run HANDLER COMMAND...: runs COMMAND, the sampler around a stand-in that waits on its
# stdin, as a coprocess with its stderr merged into its stdout, and calls HANDLER after each line it
# writes, with the line in $line and the coprocess's stdin in $to_run, which HANDLER closes with
# close_run. It leaves the output in $out, the last read's status in $read_status (1 once the
# output ended, over 128 once 60 s passed without a line) and the command's status in $status. The
# sampler writes a line every 5 s, so 60 s without one is a sampler that stopped, and the case
# fails then rather than at the job's own timeout. Once the output ends, a stdin HANDLER left open
# is closed, which lets the stand-in go whatever it was waiting for.
drive_run() {
  local handler=$1 pid from_run
  shift
  coproc RUN { exec "$@" 2>&1; }
  pid=$RUN_PID
  to_run=${RUN[1]}
  # A copy of the read end, because bash closes its own when it reaps the coprocess, which can be
  # before the coprocess's last lines are read.
  exec {from_run}<&"${RUN[0]}"
  out=""
  while :; do
    read_status=0
    IFS= read -r -t 60 line <&"$from_run" || read_status=$?
    [ "$read_status" -eq 0 ] || break
    out+=$line$'\n'
    "$handler"
  done
  close_run
  exec {from_run}<&-
  status=0
  wait "$pid" || status=$?
}

# close_run: closes drive_run's coprocess's stdin, once.
close_run() {
  [ -z "$to_run" ] || exec {to_run}>&-
  to_run=""
}

echo "case: a failing command's status passes through"
run_sampler '' sh -c 'exit 3'
check "exits 3" "$(is "$status" 3)"
check "names the default limit" "$(contains "$out" 'the limit is 8388608 kB (the default')"

echo "case: a peak over the limit fails the run and names the test that was running"
# The grower prints a -json stream naming a test as running, a parallel test that ran, paused and
# was let go on, and another left paused, then waits on its stdin, which is this case's. It grows
# past a 1 kB limit once a sample has named that test, so the stream has been read by then, and it
# ends once a later sample has seen it grown: a VmRSS above the 64 MiB it allocates. So the highest sample is one taken with the test known, and there are two
# samples at least, on a machine of any speed. A grower that slept 6 s instead was gone before
# the second walk reached it when the sampler walked the whole /proc, about a second a walk: 18
# runs of 18 took one sample among 26,800 processes at a load average of 320 to 380.
grower='
import json, sys
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
sys.stdin.readline()
block = b"x" * (64 * 1024 * 1024)
sys.stdin.read()
sys.stdout.write(str(len(block)) + "\n")
'
# on_grower_line: a sample naming the test lets the grower grow, and a later sample that saw it
# past the 64 MiB lets it end.
on_grower_line() {
  if [ "$stage" = unnamed ] && [[ $line == *'tests running: '*TestTheSamplerNamesMe* ]]; then
    echo grow >&"$to_run"
    stage=growing
  elif [ "$stage" = growing ] && [[ $line =~ \ s,\ VmRSS\ ([0-9]+)\ kB ]] &&
    [ "${BASH_REMATCH[1]}" -gt 65536 ]; then
    close_run
    stage=grown
  fi
}
stage=unnamed
drive_run on_grower_line env RACE_MEM_LIMIT_KB=1 "$sampler" python3 -c "$grower"
check "a sample named the test, and a later one saw it grown" "$(is "$stage" grown)"
check "the sampler wrote until it ended" "$(is "$read_status" 1)"
check "exits 1" "$(is "$status" 1)"
check "names the limit it was given" "$(contains "$out" 'the limit is 1 kB (RACE_MEM_LIMIT_KB)')"
decision=$(grep -- 'had these tests running' <<<"$out" || true)
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
# The grower ends only after a later sample saw it grown, so this holds by construction: it is no
# window of seconds, and a late first sample (the first walk of a loaded machine) changes nothing.
check "took two samples at least" \
  "$([ "$(grep -c -- ' s, VmRSS ' <<<"$out")" -ge 2 ] && echo true || echo false)"

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

echo "case: a sampled name's control characters are printed escaped"
# The kernel leaves every byte of a name but "\n" and "\\" as it is (race_memory.tree_statuses),
# so a name can hold a carriage return, an escape sequence or a C1 control. The Name line here is
# read through summarize, and the sample is printed both with a name read from a status text and
# with one that holds a newline, which no status text can.
escaped=$(peak '
import race_memory
walk = race_memory.summarize(["Name:\ta\rb\x1b[31m\x85\u202ec\nPPid:\t1\nVmRSS:\t    7 kB\n"], 0.0, "none")
print(walk.line())
print(race_memory.Sample(seconds=0.0, rss_kb=7, hwm_kb=None, comm="x\ny\tz", tests="none").line())
')
check "the read name prints escaped" \
  "$(contains "$escaped" '^race memory: at 0\.0 s, VmRSS 7 kB in a\\rb\\x1b\[31m\\x85\\u202ec; tests running: none$')"
check "a newline and a tab in a name print escaped" \
  "$(contains "$escaped" '^race memory: at 0\.0 s, VmRSS 7 kB in x\\ny\\tz; tests running: none$')"
check "the two samples are two lines" "$(is "$(wc -l <<<"$escaped")" 2)"
check "no raw carriage return, escape or C1 control" \
  "$(is "$(LC_ALL=C grep -c $'[\r\x1b]\\|\xc2\x85' <<<"$escaped")" 0)"

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

echo "case: the sampler reads the files of its command's own processes alone"
# tree_statuses, called on this case's own Python and one child of it, with every file read under
# /proc recorded. A walk of every process on the machine reads a status for each of them, and one
# that never answers (a FIFO mounted over a stranger's status) held the sampler to the job's own
# timeout; the walk of the tree by its children lists reads the two processes alone.
walked=$(peak '
import os, pathlib, subprocess, race_memory
child = subprocess.Popen(["sleep", "60"])
read = set()
for name in ("read_bytes", "read_text"):
    def recording(self, *args, _original=getattr(pathlib.Path, name), **kwargs):
        read.add(str(self))
        return _original(self, *args, **kwargs)
    setattr(pathlib.Path, name, recording)
try:
    statuses = race_memory.tree_statuses(os.getpid())
finally:
    child.kill()
    child.wait()
tree = {str(os.getpid()), str(child.pid)}
outside = {path.split("/")[2] for path in read if path.startswith("/proc/")} - tree
print(len(statuses), len(outside))
')
check "reads the status of the command and of its child" "$(is "${walked% *}" 2)"
check "reads no file of a process outside the tree" "$(is "${walked#* }" 0)"

# named_command <name in hex> <the name as a sample prints it, as a glob>: runs the sampler around a
# command that writes the name to its own /proc/self/comm and waits on its stdin, which the case
# closes once a sample has named it. It leaves the kernel's own Name line of the command, in hex,
# in $named. The walk whose line follows the command's "renamed" may have begun before the
# rename, but the walk after it began after, so two samples past "renamed" with no name in them
# is a failure rather than a wait without end.
named_command() {
  want=$2
  stage=unnamed
  samples_after=0
  drive_run on_renamed_line env -u RACE_MEM_LIMIT_KB "$sampler" python3 -c '
import sys
with open("/proc/self/comm", "wb") as comm:
    comm.write(bytes.fromhex(sys.argv[1]))
with open("/proc/self/status", "rb") as status:
    print("named " + status.readline().rstrip(b"\n").hex(), flush=True)
print("renamed", flush=True)
sys.stdin.read()
print("the renamed command spoke", flush=True)
' "$1"
  named=$(sed -n 's/^named //p' <<<"$out")
}
# on_renamed_line: the first sample after the command's "renamed" that names it lets it go; two
# such samples without its name let it go too, and the case fails.
on_renamed_line() {
  if [ "$stage" = unnamed ] && [ "$line" = renamed ]; then
    stage=renamed
  elif [ "$stage" = renamed ] && [[ $line == 'race memory: at '* ]]; then
    samples_after=$((samples_after + 1))
    # shellcheck disable=SC2053 # $want is a glob on purpose
    if [[ $line == *" in "${want}[,\;]* ]]; then
      stage=named
      close_run
    elif [ "$samples_after" -ge 2 ]; then
      close_run
      stage=unseen
    fi
  fi
}

echo "case: the command's own process, named in bytes that are not UTF-8, is named in a sample"
# b"\xff\xfeA3": a strict decode of its status raised UnicodeDecodeError at the name's 0xff.
named_command fffe4133 $'\xef\xbf\xbd\xef\xbf\xbdA3'
check "the kernel shows the name's bytes as written" "$(is "$named" 4e616d653a09fffe4133)"
check "a sample names the command with U+FFFD for each byte that is not UTF-8" "$(is "$stage" named)"
check "exits 0" "$(is "$status" 0)"
check "passes the command's own output through" "$(contains "$out" '^the renamed command spoke$')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a process whose name holds a line break other than a newline passes for no field"
# b"a\x1eVmRSS:\tz": str.splitlines() broke the Name line at \x1e, and int("z") raised for the
# VmRSS it seemed to give. The sample prints the name escaped.
named_command 611e566d5253533a097a 'a\\x1eVmRSS:\\tz'
check "the kernel shows the name's bytes as written" "$(is "$named" 4e616d653a09611e566d5253533a097a)"
check "a sample names the command" "$(is "$stage" named)"
check "exits 0" "$(is "$status" 0)"
last=${out%$'\n'}
check "its last line gives the peak and the limit" \
  "$(contains "${last##*$'\n'}" '^race memory: the peak was [0-9][0-9]* kB (Vm.*), the limit 8388608 kB$')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a byte of the command's stdout that is not UTF-8 is read as U+FFFD"
run_sampler '' printf 'before\n\377\nafter\n'
check "exits 0" "$(is "$status" 0)"
check "the line before it shows" "$(contains "$out" '^before$')"
check "the byte shows as U+FFFD" "$(contains "$out" $'^\xef\xbf\xbd$')"
check "the line after it shows" "$(contains "$out" '^after$')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: where the kernel lists no children, a sample finds the descendants with pgrep -P"
# main as on a kernel built without CONFIG_PROC_CHILDREN (children_listed false), with pgrep given
# 60 s (PGREP_SECONDS), so its own bound decides nothing here. The command is a shell whose child
# holds 96 MiB until this case closes its stdin, under a 64 MiB limit: only a sample that sees the
# child can fail the step. The case lets the child go once a sample has seen it past the limit,
# or after three samples that did not. The same run with pgrep finding no child, as the sampler
# was before it fell back to pgrep, passes after its three samples: the blindness the fallback
# closes.
no_lists='
import sys, race_memory as r
r.children_listed = lambda pid: False
r.PGREP_SECONDS = 60.0
if sys.argv[1] == "blind":
    r.pgrep_children = lambda pid: []
sys.exit(r.main(sys.argv[2:]))
'
big_child='
import sys
held = bytearray(96 * 1024 * 1024)
for i in range(0, len(held), 4096):
    held[i] = 1
sys.stdin.read()
'
# on_child_line: a sample of the child past the limit lets it go, and so do three samples without.
on_child_line() {
  if [[ $line =~ \ s,\ VmRSS\ ([0-9]+)\ kB\ in\ python3 ]] && [ "${BASH_REMATCH[1]}" -gt 65536 ]; then
    seen=true
    close_run
  elif [[ $line == 'race memory: at '* ]]; then
    samples=$((samples + 1))
    [ "$samples" -lt 3 ] || close_run
  fi
}
seen=false samples=0
drive_run on_child_line env RACE_MEM_LIMIT_KB=65536 PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" \
  python3 -c "$no_lists" pgrep sh -c 'python3 -c "$1"; :' sh "$big_child"
check "says it finds the descendants with pgrep -P" "$(contains "$out" 'finds the command.s descendants with pgrep -P')"
check "a sample saw the child past the limit" "$(is "$seen" true)"
check "exits 1" "$(is "$status" 1)"
check "says the peak passed the limit" "$(contains "$out" 'passed the limit of 65536 kB')"
seen=false samples=0
drive_run on_child_line env RACE_MEM_LIMIT_KB=65536 PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" \
  python3 -c "$no_lists" blind sh -c 'python3 -c "$1"; :' sh "$big_child"
check "with pgrep finding no child, three samples and more never see it, and the same run passes" \
  "$([ "$seen,$status" = false,0 ] && [ "$samples" -ge 3 ] && echo true || echo false)"

echo "case: a pgrep that fails makes the step fail, naming the samples that missed the tree"
failing_pgrep=$(mktemp -d)
printf '#!/bin/sh\necho "pgrep: cannot read /proc" >&2\nexit 2\n' >"$failing_pgrep/pgrep"
chmod +x "$failing_pgrep/pgrep"
set +e
out=$(env -u RACE_MEM_LIMIT_KB PATH="$failing_pgrep:$PATH" PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" \
  python3 -c "$no_lists" pgrep sleep 1 2>&1)
status=$?
set -e
rm -rf -- "$failing_pgrep"
check "exits 1" "$(is "$status" 1)"
check "names pgrep's failure" "$(contains "$out" 'a sample saw only part of the tree: pgrep -P [0-9]* exited 2')"
check "says the limit was not checked against the whole tree" \
  "$(contains "$out" 'samples could not read the command.s descendants')"

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
# whole, then holds the rest of its line, so a sample written straight after the fragment would be
# glued to it, one line as long as both. (Plain output cannot do this: the script reads the
# command's stdout a line at a time.) The stand-in holds the rest of its line on its stdin, which
# this case closes once the sampler's own lines show two samples after the fragment: the wait is on
# those lines, not on a clock.
fragment='
import json, sys
def event(**fields):
    sys.stdout.write(json.dumps(fields) + "\n")
    sys.stdout.flush()
event(Action="run", Test="TestAPrintWithNoNewline")
event(Action="output", Test="TestAPrintWithNoNewline", Output="partial output with no newline")
sys.stdin.read()
event(Action="output", Test="TestAPrintWithNoNewline", Output="\n")
event(Action="pass", Test="TestAPrintWithNoNewline")
'
# on_fragment_line: once the fragment is out, a second sample after it lets the stream go on.
on_fragment_line() {
  if [ "$stage" = waiting ] && [ "$line" = 'partial output with no newline' ]; then
    stage=after
    samples_after=0
  elif [ "$stage" = after ] && [[ $line == 'race memory: at '*' s, VmRSS '* ]]; then
    samples_after=$((samples_after + 1))
    if [ "$samples_after" -ge 2 ]; then
      close_run
      stage=released
    fi
  fi
}
stage=waiting
drive_run on_fragment_line env -u RACE_MEM_LIMIT_KB "$sampler" python3 -c "$fragment"
check "two samples followed the fragment before the stream went on" "$(is "$stage" released)"
check "exits 0" "$(is "$status" 0)"
check "the fragment keeps its own line" "$(contains "$out" '^partial output with no newline$')"
check "the sample after it starts its own line" \
  "$(contains "$(grep -A1 -- '^partial output with no newline$' <<<"$out" | tail -n 1)" '^race memory: at [0-9.]* s, VmRSS ')"
check "no sample is glued to the fragment" "$(is "$(contains "$out" 'newline.*race memory')" false)"

echo "case: a process on the machine whose name is no UTF-8 does not end the run"
# This sampler reads the files of its command's own processes alone (the case on its own processes
# above), so another process's status is never read and its name cannot end a walk. The case keeps
# the guard a walk of every process would need: were the sampler to read the whole machine again, a
# process that names itself with bytes that are no UTF-8 (prctl PR_SET_NAME) would be read, and a
# strict decode of it would end the step. The process stays up while the sampler takes its first
# sample; the wait for it to be up is a bound on a readiness file, and the control below fails the
# case if it never came, so no verdict rests on that bound.
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
