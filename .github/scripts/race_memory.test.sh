#!/usr/bin/env bash
# Tests for `race_memory.py`: a command's status passes through, a peak over the limit fails the
# step and names the tests that were running, a run within the limit ends with the peak, a
# `go test -json` stream's output reaches the log, the sampler reads the files of its command's
# own processes alone, and neither a process's name nor a byte of the command's stdout that is not
# UTF-8 stops a run before it gives its result.
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
# The grower prints a -json event naming a test as running, then waits on its stdin, which is
# this case's. It grows past a 1 kB limit once a sample has named that test, so the stream has
# been read by then, and it ends once a later sample has seen it grown: a VmRSS above the 64 MiB
# it allocates. So the highest sample is one taken with the test known, and there are two
# samples at least, on a machine of any speed. A grower that slept 6 s instead was gone before
# the second walk reached it where one walk of /proc takes about a second: 18 runs of 18 took one
# sample among 26,800 processes at a load average of 320 to 380 (dispatch://INFRA-3904).
grower='
import json, sys
sys.stdout.write(json.dumps({"Action": "run", "Test": "TestTheSamplerNamesMe"}) + "\n")
sys.stdout.flush()
sys.stdin.readline()
block = b"x" * (64 * 1024 * 1024)
sys.stdin.read()
sys.stdout.write(str(len(block)) + "\n")
'
# on_grower_line: a sample naming the test lets the grower grow, and a later sample that saw it
# past the 64 MiB lets it end.
on_grower_line() {
  if [ "$stage" = unnamed ] && [[ $line == *'tests running: TestTheSamplerNamesMe'* ]]; then
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
check "its decision line names the tests at the highest sample" \
  "$(contains "$out" 'had these tests running: TestTheSamplerNamesMe')"
check "says the peak passed the limit" "$(contains "$out" 'passed the limit of 1 kB')"
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

summary "race_memory.py"
