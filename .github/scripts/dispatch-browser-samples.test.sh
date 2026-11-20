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

# await_tick <pid>: waits until the sampler <pid> has written a whole tick, that is until the
# command it runs is its `sleep 5`, which it starts only once both files hold that tick, or until
# it is gone; sets `child` to that sleep's pid, or to nothing when the sampler is gone. A tick
# reads every process of the machine (`ps -eo`), which takes seconds on a devbox that runs 15,000
# of them and longer under load, so the wait is for the tick itself, not for a time a tick is
# assumed to take, and the case that calls it checks `child` by name. Its one bound, 300 s, is for
# a sampler that hangs before its sleep. It reads /proc with builtins alone.
await_tick() {
  local pid=$1 c comm stat state i
  local -a kids
  child=
  for ((i = 0; i < 3000; i++)); do
    stat=
    { IFS= read -r stat <"/proc/$pid/stat"; } 2>/dev/null || true
    state=${stat##*) }
    state=${state%% *}
    # Gone, or a zombie its parent has not reaped yet: it writes nothing more.
    { [ -n "$stat" ] && [ "$state" != Z ]; } || return 0
    kids=()
    { read -r -a kids <"/proc/$pid/task/$pid/children"; } 2>/dev/null || true
    for c in ${kids[@]+"${kids[@]}"}; do
      comm=
      { read -r comm <"/proc/$c/comm"; } 2>/dev/null || true
      if [ "$comm" = sleep ]; then
        child=$c
        return 0
      fi
    done
    sleep 0.1
  done
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
# the shape it writes is worth holding. It is started as the browser steps start it, left until it
# has written a whole tick (await_tick), and killed as their trap kills it.
dir=$(fixture sampler-runs)
# What a previous run left behind. The sampler truncates both files, so neither line survives
# into the reading.
printf 'stale\n' > "$dir/memory-samples.txt"
printf 'machine 999999999\n' > "$dir/cpu-samples.txt"
"$samples_script" sample "$dir" &
sampler=$!
await_tick "$sampler"
check "it wrote a whole tick and reached its sleep" "$(is "${child:+yes}" yes)"
kill "$sampler" 2>/dev/null || true
wait "$sampler" 2>/dev/null || true
check "the memory samples are numbers of kB" \
  "$(is "$(grep -cEv '^[0-9]+$' "$dir/memory-samples.txt")" 0)"
check "it took at least one memory sample" \
  "$(is "$(awk 'END { print (NR >= 1) ? "yes" : "no" }' "$dir/memory-samples.txt")" yes)"
check "the first CPU line of a tick is the machine's busy total" \
  "$(contains "$(head -n 1 "$dir/cpu-samples.txt")" '^machine [0-9][0-9]*$')"
check "every other CPU line is a pid, its ticks and its group" \
  "$(is "$(grep -cEv '^(machine [0-9]+|[0-9]+ [0-9]+ (browser|runner|dispatch|postgres|bun))$' "$dir/cpu-samples.txt")" 0)"
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

echo "case: each browser step's own sampler lines, run under the step script's shell options, end on the suite's status"
# The trap and the stop of the two browser steps' scripts (dispatch-browser-shard-suite.sh and
# dispatch-browser-alone-suite.sh, which dispatch-browser.yaml's shard and alone steps run), cut
# from those scripts, run around a real sampler under `set -euo pipefail`, the scripts' own
# options. A trap command that fails there (a kill of the sampler the stop has already reaped,
# with no `|| true`) ends the step with 1 after a passing suite, which is how run 44622560351
# failed every browser leg.
suites=("$script_dir/dispatch-browser-shard-suite.sh" "$script_dir/dispatch-browser-alone-suite.sh")
traps=() stops=()
for suite in "${suites[@]}"; do
  mapfile -t found < <(awk '/^ *trap .kill "\$sampler"/ { sub(/^ */, ""); print }' "$suite")
  traps+=("${found[@]}")
  mapfile -t found < <(awk '/^ *kill "\$sampler" / { sub(/^ */, ""); print }' "$suite")
  stops+=("${found[@]}")
done
check "the two browser steps' scripts each have one trap and one stop" "$(is "${#traps[@]}:${#stops[@]}" 2:2)"
for i in 0 1; do
  for want in 0 3; do
    dir=$(fixture "step-$i-$want")
    set +e
    bash -c "set -euo pipefail
\"\$1\" sample \"\$2\" & sampler=\$!
${traps[$i]}
sleep 0.3
${stops[$i]}
exit $want" step "$samples_script" "$dir" >/dev/null 2>&1
    status=$?
    set -e
    check "step $((i + 1)) with a suite status of $want exits $want" "$(is "$status" "$want")"
  done
done

echo "case: each browser step's stop leaves neither the sampler nor a command it started running"
# The reading after the stop reads the two files the sampler writes, so the stop waits for the
# sampler (`wait "$sampler"`), and the sampler's SIGTERM trap ends the command under way (its 5 s
# sleep, or its tick's ps and awk) and waits for it before the sampler exits: one left running would
# go on writing after the step's wait has returned. Each step's stop line, cut from its script as
# above, runs against a real sampler caught in its sleep.
for i in 0 1; do
  dir=$(fixture "stop-$i")
  "$samples_script" sample "$dir" &
  sampler=$!
  await_tick "$sampler"
  check "step $((i + 1))'s sampler reached its sleep" "$(is "${child:+yes}" yes)"
  eval "${stops[$i]}"
  check "step $((i + 1))'s stop leaves no sampler" "$(is "$(kill -0 "$sampler" 2>/dev/null && echo alive || echo gone)" gone)"
  # A zombie runs nothing and writes nothing: gone.
  state=$(awk '{ print $3 }' "/proc/$child/stat" 2>/dev/null || true)
  [ "$state" != Z ] || state=
  check "and no sleep the sampler had started" "$(is "${state:-gone}" gone)"
done

echo "case: a tick whose process walk never ends is cut at its 20 s, and holds neither the sampler nor its stop"
# The CPU reading walks every process of the machine (ps -eo), which a loaded machine can hold for
# long. A ps that does not end for 120 s stands in for one here, first on PATH, writing its own pid
# before it sleeps. The sampler must go on to its next tick, write no CPU line for the tick it cut,
# name the cut, and end within moments of the stop's SIGTERM, the stand-in gone.
dir=$(fixture slow-walk)
mkdir -p "$dir/bin"
printf '#!/bin/sh\necho "$$" >>"%s/ps-pids"\nexec sleep 120\n' "$dir" >"$dir/bin/ps"
chmod +x "$dir/bin/ps"
# Both files exist before the sampler starts, so no read below fails before it truncates its own.
: >"$dir/memory-samples.txt"
: >"$dir/ps-pids"
PATH="$dir/bin:$PATH" "$samples_script" sample "$dir" 2>"$dir/stderr" &
sampler=$!
# The second memory sample is written only once the first tick's CPU reading has ended: at its cut
# after 20 s, or after the stand-in's 120 s with no cut. Read with builtins, for up to 60 s.
lines=()
for ((i = 0; i < 600; i++)); do
  mapfile -t lines <"$dir/memory-samples.txt"
  [ "${#lines[@]}" -lt 2 ] || break
  sleep 0.1
done
check "the sampler went on to its next tick within 60 s" "$(is "$((${#lines[@]} >= 2))" 1)"
check "the cut tick wrote no CPU line" "$(is "$(wc -l <"$dir/cpu-samples.txt")" 0)"
check "and the cut is named" "$(contains "$(<"$dir/stderr")" "had not ended within its 20 s")"
# The stop lands in the second tick's reading, the stand-in again: the trap ends it at once.
began=$EPOCHREALTIME
kill "$sampler" 2>/dev/null || true
wait "$sampler" 2>/dev/null || true
took=$(awk -v a="$began" -v b="$EPOCHREALTIME" 'BEGIN { printf "%d", b - a }')
check "the stop ends within 5 s, not at the reading's end (it took ${took} s)" "$(is "$((took <= 5))" 1)"
left=
while read -r pid; do
  # A zombie runs nothing: gone.
  state=$(awk '{ sub(/.*\) /, ""); print $1 }' "/proc/$pid/stat" 2>/dev/null || true)
  [ -z "$state" ] || [ "$state" = Z ] || left="$left $pid"
done <"$dir/ps-pids"
check "and no ps stand-in is left running" "$(is "${left:-none}" none)"

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
