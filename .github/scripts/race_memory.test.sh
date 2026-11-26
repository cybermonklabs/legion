#!/usr/bin/env bash
# Tests for `race_memory.py`: a command's status passes through, a peak over the limit fails the
# step and names the tests that were running, read from the file the command's `--jsonfile`
# names as it grows, a run within the limit ends with the peak, the command's own output passes
# through as it stands, and a command given no `--jsonfile` is refused.
#
# Each case runs the real script around a stand-in command — a shell or a Python program that
# writes a -json stream to the file its `--jsonfile` names and grows — so no Go, no race binary
# and no database are needed; .github/scripts/go-test.test.sh runs it around gotestsum itself.
# The two functions the sampling loop reads its numbers with, `summarize` and `choose_peak`, and the
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

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# peak <python expression over race_memory>: the peak functions called with no process involved.
peak() {
  env PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 -c "$1"
}

echo "case: a failing command's status passes through"
run_sampler '' sh -c 'exit 3' sh --jsonfile "$work/exit.json"
check "exits 3" "$(is "$status" 3)"
check "names the default limit" "$(contains "$out" 'the limit is 8388608 kB (the default')"
check "names the file it reads the running tests from" \
  "$(contains "$out" "the tests running are read from $work/exit.json")"

echo "case: a peak over the limit fails the run and names the test that was running"
# Writes a -json event naming a test as running to its jsonfile, then grows past a 1 kB limit and
# lives until the case has seen the sampler take its second sample (the first at 0 s, the next 5 s
# later, by when the file holds the event), and only then ends: the case waits on the sampler's
# own lines, not on a clock, so a sampler a loaded runner holds back still samples the grown
# process. The 60 s bounds only end a run whose sample never comes, which the checks then report.
release=$(mktemp -u)
grower='
import json, os, sys, time
with open(sys.argv[3], "w") as jsonfile:
    jsonfile.write(json.dumps({"Action": "run", "Test": "TestTheSamplerNamesMe"}) + "\n")
block = b"x" * (64 * 1024 * 1024)
deadline = time.monotonic() + 60
while not os.path.exists(sys.argv[1]) and time.monotonic() < deadline:
    time.sleep(0.05)
sys.stdout.write(str(len(block)) + "\n")
'
sampled=$(mktemp)
env "RACE_MEM_LIMIT_KB=1" "$sampler" python3 -c "$grower" "$release" --jsonfile "$work/grower.json" > "$sampled" 2>&1 &
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
check "took a sample at 0 s and another at 5 s" \
  "$([ "$(grep -c -- ' s, VmRSS ' <<<"$out")" -ge 2 ] && echo true || echo false)"

echo "case: a run within the limit exits 0 and ends with the peak"
run_sampler '' sh -c 'echo the command spoke' sh --jsonfile "$work/spoke.json"
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

echo "case: the command's standard output passes through as it stands"
# gotestsum's standard-verbose format is text, so a line is written as the command wrote it,
# a line that looks like a -json event included: the events are the jsonfile's.
stream='
import json, sys
sys.stdout.write("--- FAIL: TestA (0.00s)\n")
sys.stdout.write(json.dumps({"Action": "output", "Test": "TestA", "Output": "not unwrapped\n"}) + "\n")
'
run_sampler '' python3 -c "$stream" --jsonfile "$work/stream.json"
check "exits 0" "$(is "$status" 0)"
check "the --- FAIL line shows" "$(contains "$out" '^--- FAIL: TestA (0.00s)$')"
check "a line shaped like a -json event shows whole" "$(contains "$out" '^{"Action": "output"')"

echo "case: a command given no --jsonfile is refused, and not run"
run_sampler '' sh -c 'echo ran'
check "exits 2" "$(is "$status" 2)"
check "says the running tests are read from the command's --jsonfile" \
  "$(contains "$out" 'is given no --jsonfile, and the tests running at each sample are read from that file')"
check "runs nothing" "$(is "$(contains "$out" '^ran$')" false)"
run_sampler '' gotestsum --packages ./... -- --jsonfile after-the-separator.json
check "a --jsonfile after the command's -- is go test's, and is refused too" "$(is "$status" 2)"

summary "race_memory.py"
