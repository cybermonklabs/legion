#!/usr/bin/env bash
# Tests for `dispatch-browser-samples.sh`: the browser steps of dispatch-browser.yaml end on two
# `::notice::` lines a person reads off the run, so each case lays down a samples file whose
# numbers are known and asserts the line the script prints from it.
#
# The two readings are pure functions of those two files - the lowest MemAvailable over the
# memory samples, and the per-group CPU split over the CPU samples - which is what makes them
# testable here: the fixtures stand in for what a tick of the sampler wrote on a runner. The
# `sample` mode is driven for real once, over this machine's own /proc, since what it writes is
# what every other case reads.
#
# NO CASE RUNS A BROWSER, A SUITE OR A NETWORK CALL: each one is a file, /proc and the script.
#
# Run from anywhere: .github/scripts/dispatch-browser-samples.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture is built inside `dir=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a directory that failed to build is used anyway
# and the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
samples_script="$script_dir/dispatch-browser-samples.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: an empty sample directory for one case, the shape $RUNNER_TEMP has when the
# sampler has not run yet. Echoes its path. A repeated name is refused here rather than
# surfacing as one case reading another's samples.
fixture() {
  local dir="$work/$1"
  if [ -e "$dir" ]; then
    echo "two cases named $1" >&2
    return 1
  fi
  mkdir -p "$dir"
  echo "$dir"
}

# run_report <dir> <title>: runs the reading over that case's samples, setting `out` to its
# combined output and `status` to its exit code. Not a subshell, so both survive the call.
run_report() {
  set +e
  out=$("$samples_script" report "$1" "$2" 2>&1)
  status=$?
  set -e
}

echo "case: the lowest MemAvailable, over the samples that are numbers"
dir=$(fixture memory-low)
# A tick writes one number per line. The blank line is the short write of a sampler killed
# mid-tick, which is a reading of nothing and is not counted.
printf '4000000\n3800000\n3500000\n\n3900000\n' > "$dir/memory-samples.txt"
: > "$dir/cpu-samples.txt"
run_report "$dir" "shard 1"
check "exits 0" "$(is "$status" 0)"
check "names the lowest value and the sample count" \
  "$(contains "$out" '^::notice title=shard 1 MemAvailable::the lowest MemAvailable during the browser step was 3500000 kB, over 4 samples$')"

echo "case: the CPU split, group by group"
# Two ticks. The machine line of each is its busy total, so the step's busy time is 40000 ticks,
# or 400 CPU-seconds. Each process's share is the difference between its two readings, except
# postgres, which the second tick first saw and which therefore counts whole.
dir=$(fixture cpu-split)
: > "$dir/memory-samples.txt"
cat > "$dir/cpu-samples.txt" <<'SAMPLES'
machine 100000
101 1000 browser
102 500 runner
103 0 dispatch
104 300 bun
machine 140000
101 21000 browser
102 4500 runner
103 2000 dispatch
104 300 bun
105 2000 postgres
SAMPLES
run_report "$dir" "shard 1"
check "exits 0" "$(is "$status" 0)"
check "the busy total, the sample count, every group and what is left" \
  "$(contains "$out" '^::notice title=shard 1 CPU::the browser step used 400 CPU-seconds over 2 samples: browser 200 (50%), runner 40 (10%), dispatch 20 (5%), postgres 20 (5%), bun 0 (0%), everything else 120 (30%)$')"

echo "case: a process the first tick already saw is counted from that tick on, not from zero"
# The one reading the first tick holds is the CPU that process spent before the step; counting
# it would charge the step with the runner's own boot.
dir=$(fixture cpu-baseline)
: > "$dir/memory-samples.txt"
printf 'machine 0\n7 90000 bun\nmachine 10000\n7 91000 bun\n' > "$dir/cpu-samples.txt"
run_report "$dir" "shard 2"
check "the group holds the difference, not the whole reading" \
  "$(contains "$out" 'bun 10 (10%), everything else 90 (90%)$')"

echo "case: a pid read lower than last tick adds nothing"
# A pid the kernel gave to another process after the first one exited. Its reading restarts from
# that process's own ticks, which is not CPU the step spent twice and not a negative share.
dir=$(fixture cpu-pid-reuse)
: > "$dir/memory-samples.txt"
printf 'machine 1000\n7 500 bun\nmachine 2000\n7 100 bun\n' > "$dir/cpu-samples.txt"
run_report "$dir" "shard 3"
check "the group is unchanged" "$(contains "$out" 'bun 0 (0%), everything else 10 (100%)$')"

echo "case: the title is the leg the caller names"
dir=$(fixture title-alone)
printf '2000000\n' > "$dir/memory-samples.txt"
printf 'machine 500\nmachine 1500\n' > "$dir/cpu-samples.txt"
run_report "$dir" webkit-alone
check "on the memory reading" "$(contains "$out" '^::notice title=webkit-alone MemAvailable::')"
check "and on the CPU reading" "$(contains "$out" '^::notice title=webkit-alone CPU::')"

echo "case: a title is escaped as GitHub escapes a property, and in that order"
# `,` would otherwise end the property and cut the title short, and `%` must be replaced before
# the other two, or it re-encodes the `%` they write: swapping those lines gives `p%25%252Cq%253Ar`,
# which the runner unescapes back to `p%2Cq%3Ar` rather than to the name. A single `:` ends
# nothing, and is encoded because GitHub's property table holds it.
dir=$(fixture title-escaped)
printf '2000000\n' > "$dir/memory-samples.txt"
printf 'machine 500\nmachine 1500\n' > "$dir/cpu-samples.txt"
run_report "$dir" 'p%,q:r'
check "on the memory reading" "$(contains "$out" '^::notice title=p%25%2Cq%3Ar MemAvailable::')"
check "and on the CPU reading" "$(contains "$out" '^::notice title=p%25%2Cq%3Ar CPU::')"

echo "case: no samples is a warning, not a reading of zero"
# A step whose sampler died at once must not report 0 kB and 0 CPU-seconds as if it had
# measured them.
dir=$(fixture samples-empty)
: > "$dir/memory-samples.txt"
: > "$dir/cpu-samples.txt"
run_report "$dir" "shard 4"
check "exits 0" "$(is "$status" 0)"
check "says the memory reading took no sample" "$(contains "$out" '^::warning::the browser step took no MemAvailable sample$')"
check "says the CPU reading took no sample" "$(contains "$out" '^::warning::the browser step took no CPU sample$')"
check "and prints no notice" "$(is "$(grep -c '::notice' <<<"$out")" 0)"

dir=$(fixture samples-absent)
run_report "$dir" "shard 4"
check "a sampler that never wrote its files is the same" "$(is "$status" 0)"
check "memory" "$(contains "$out" 'took no MemAvailable sample$')"
check "CPU" "$(contains "$out" 'took no CPU sample$')"

echo "case: the sampler writes both files, over this machine's own /proc"
# The one case that runs the sampler itself: the fixtures above stand in for what it writes, so
# the shape it writes is worth holding. It is started as the browser steps start it, left to
# complete its first tick, and killed as their trap kills it.
#
# It waits for the tick, not for a clock. A tick is one `ps` over every process of the machine
# and one awk over its lines, and on a loaded machine that is seconds: one tick's `ps` took 3.17 s
# at load 812 over 19,145 processes, so the 2 s this case slept once read the CPU file before
# its first line, and failed "the first CPU line of a tick is the machine's busy total" in 4 of 8
# runs. Nor does the first `machine` line mark a tick done: awk writes its lines through a block
# buffer, and a tick of that machine is 2,107 lines (42 KB), so the file holds the machine line
# and part of the tick long before the tick ends. What marks it done is the second memory sample,
# which the sampler's loop writes only after the first tick's ps and awk have exited and its
# 5-s sleep has passed. The loop's 600 tries of 0.5 s are a hang guard for a sampler that never
# completes a tick, which the first check below then names; no verdict rests on them.
dir=$(fixture sampler-runs)
# What a previous run left behind. The sampler truncates both files, so neither line survives
# into the reading.
printf 'stale\n' > "$dir/memory-samples.txt"
printf 'machine 999999999\n' > "$dir/cpu-samples.txt"
"$samples_script" sample "$dir" &
sampler=$!
ticked=no
for _ in $(seq 600); do
  # The numeric lines alone: the stale line is not one, and the sampler truncates the file before
  # it writes its first.
  if [ "$(grep -cE '^[0-9]+$' "$dir/memory-samples.txt" 2>/dev/null || true)" -ge 2 ]; then
    ticked=yes
    break
  fi
  sleep 0.5
done
kill "$sampler" 2>/dev/null || true
wait "$sampler" 2>/dev/null || true
check "it completed its first tick, which a second memory sample marks" "$(is "$ticked" yes)"
check "the memory samples are numbers of kB" \
  "$(is "$(grep -cEv '^[0-9]+$' "$dir/memory-samples.txt")" 0)"
check "the first CPU line of a tick is the machine's busy total" \
  "$(contains "$(head -n 1 "$dir/cpu-samples.txt")" '^machine [0-9][0-9]*$')"
# Over the first tick's lines alone, up to the second `machine` line: the kill above ends the
# sampler's shell and not the ps and awk of a tick it had started, so a later tick may still be
# half written when this reads.
check "every CPU line of the first tick is a pid, its ticks and its group" \
  "$(is "$(awk '/^machine / { n++ } n == 1' "$dir/cpu-samples.txt" |
    grep -cEv '^(machine [0-9]+|[0-9]+ [0-9]+ (browser|runner|dispatch|postgres|bun))$')" 0)"
check "the stale memory sample is gone" "$(is "$(grep -c stale "$dir/memory-samples.txt" || true)" 0)"
check "and the stale CPU sample with it" "$(is "$(grep -c 999999999 "$dir/cpu-samples.txt" || true)" 0)"
# The reading over what it just wrote: the numbers are the machine's, so what is asserted is
# that it reads at all and ends with the two lines a step prints.
run_report "$dir" "shard 1"
check "the reading over its own samples exits 0" "$(is "$status" 0)"
check "and prints the memory reading" \
  "$(contains "$out" '^::notice title=shard 1 MemAvailable::the lowest MemAvailable during the browser step was [0-9][0-9]* kB, over [0-9][0-9]* samples$')"
check "and the CPU reading, with its groups" \
  "$(contains "$out" '^::notice title=shard 1 CPU::the browser step used [0-9][0-9]* CPU-seconds over [0-9][0-9]* samples: browser .*everything else .*$')"

echo "case: the Dispatch server is counted under dispatch, by the program it runs"
# Stand-ins named as run-server.sh execs the server: the path the browser jobs'
# DISPATCH_E2E_SERVER_BIN names, which the pattern before this case fell through to `everything
# else`, and the `./dispatch-e2e` a local run builds. The first is a copy of bash spinning, so the
# reading has CPU to give it; the broker's `cipherd` beside it in the same directory is not the
# server and must stay out. The case waits until both servers are in two ticks, the second five
# seconds after the first, so the spinner's second reading is a difference over a whole tick. It
# waits on the file rather than a clock, since awk writes a tick's lines when it exits and a fixed
# sleep could read a tick half written; the 30 s bound only ends a wait for lines that never come,
# which the checks then report.
dir=$(fixture dispatch-server)
mkdir -p "$dir/out/dispatch-e2e-broker"
cp "$(command -v bash)" "$dir/out/dispatch-e2e-broker/dispatch"
cp "$(command -v sleep)" "$dir/out/dispatch-e2e-broker/cipherd"
cp "$(command -v sleep)" "$dir/dispatch-e2e"
"$dir/out/dispatch-e2e-broker/dispatch" -c 'while :; do :; done' &
server=$!
"$dir/out/dispatch-e2e-broker/cipherd" 60 &
broker=$!
(cd "$dir" && exec ./dispatch-e2e 60) &
local_server=$!
"$samples_script" sample "$dir" &
sampler=$!
# lines_of <pid>: how many dispatch lines the samples hold for that process so far.
lines_of() { grep -c "^$1 [0-9][0-9]* dispatch$" "$dir/cpu-samples.txt" 2>/dev/null || true; }
for _ in $(seq 150); do
  [ "$(lines_of "$server")" -ge 2 ] && [ "$(lines_of "$local_server")" -ge 2 ] && break
  sleep 0.2
done
kill "$sampler" "$server" "$broker" "$local_server" 2>/dev/null || true
wait "$sampler" "$server" "$broker" "$local_server" 2>/dev/null || true
check "the server at the browser jobs' path is a dispatch line of two ticks" \
  "$(is "$([ "$(lines_of "$server")" -ge 2 ] && echo yes)" yes)"
check "and so is the server a local run builds" \
  "$(is "$([ "$(lines_of "$local_server")" -ge 2 ] && echo yes)" yes)"
check "and the broker's command beside it is no line at all" \
  "$(is "$(grep -c "^$broker " "$dir/cpu-samples.txt" || true)" 0)"
# The reading over the machine's lines and the spinning server's alone: on a machine that runs
# other Dispatch servers, as a workstation does, the whole reading's `dispatch` would be theirs
# too, so only this narrowed one says the stand-in's CPU reached the group.
mkdir -p "$dir/server-only"
: > "$dir/server-only/memory-samples.txt"
grep -E "^(machine |$server )" "$dir/cpu-samples.txt" > "$dir/server-only/cpu-samples.txt" || true
run_report "$dir/server-only" "shard 1"
check "the reading gives dispatch the CPU the server spent" \
  "$(contains "$out" 'samples: browser 0 (0%), runner 0 (0%), dispatch [1-9][0-9]* ([0-9]*%), postgres 0 ')"

echo "case: a call the script cannot read"
set +e
out=$("$samples_script" 2>&1); status=$?
check "no argument fails" "$(is "$status" 1)"
check "and says what the modes are" "$(contains "$out" 'sample <dir>')"
out=$("$samples_script" sample 2>&1); status=$?
check "a mode with no directory fails" "$(is "$status" 1)"
out=$("$samples_script" report "$work" 2>&1); status=$?
check "a reading with no title fails" "$(is "$status" 1)"
check "and says the title is wanted" "$(contains "$out" 'report <dir> <title>')"
out=$("$samples_script" summarise "$work" x 2>&1); status=$?
check "an unknown mode fails" "$(is "$status" 1)"
out=$("$samples_script" sample "$work/no-such-directory" 2>&1); status=$?
check "sampling into a directory that is not there fails" "$(is "$status" 1)"
check "naming it" "$(contains "$out" 'no-such-directory is not a directory')"
set -e

summary "dispatch-browser-samples.sh"
