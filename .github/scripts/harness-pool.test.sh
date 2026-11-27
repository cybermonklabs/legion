#!/usr/bin/env bash
# Tests for `harness-pool.sh`: harnesses started by one step run beside each other, never more at
# once than the slots and in the order queued, each in its own session with the starting step's
# environment and working directory and SIGINT at its default; starts at the same moment take
# entries of their own; a harness's CPU seconds are its own, and unknown rather than 0 when they
# cannot be read; a harness started with --minutes sees its deadline and is stopped at its bound;
# each line of a harness's log is stamped with the time it was written, a last line without a
# newline included, the record says whether it covers the whole log, and nothing the record does
# reaches a harness's verdict, its CPU seconds, its slot or the wait's; and the wait is the
# verdict on every one of them, a failure, a harness that signals its own process group, a
# recorder that was killed (before or after its harness started, or replaced by another process
# of its pid), and a wait stopped by an INT or a TERM included, and on none started.
#
# Every case drives the script CI runs, in a pool directory of its own. The harnesses are small
# bash commands that wait on marker files the case writes, and each wait for a file gives up after
# 20 s and fails its case by name rather than hanging. Ten checks lean on a clock. Nine lean in
# the direction where a slow machine can only let a broken pool pass, never fail a working one:
# the slot case gives a pool that ignored the slot 1 s to start a harness it must not, and the
# drain case gives a wait that did not wait for a status 1 s to judge without it; the bound case
# shortens a minute to one second through HARNESS_POOL_SECONDS_PER_MINUTE and requires that a
# harness which would sleep 300 s is stopped; the CPU case requires a loop of a million
# iterations to cost more than 0.3 s of CPU and a sleep to cost less, and the stamper's CPU case
# requires a harness that sleeps beside a stamper that ran that loop to cost less, CPU seconds
# that a busy machine does not add to; and the three line-time gaps require a second's sleep
# between two lines to show as half a second or more between their stamps, which a record
# written at the harness's end could not show at all. The tenth leans the other way: a last line
# without a newline, printed three seconds before its harness ends, must carry a time a second or
# more before that end, which a stamper that read it more than two seconds late would not give.
#
# Run from anywhere: .github/scripts/harness-pool.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

pool="$script_dir/harness-pool.sh"
work=$(mktemp -d)
# What a failed case left running: each harness's session, by the id the harness published, and
# each recorder, whose command line names its entry under $work.
cleanup() {
  local session
  for session in "$work"/*/pool/*.session; do
    if [[ -e $session ]]; then
      pkill -KILL -s "$(< "$session")" 2> /dev/null || true
    fi
  done
  pkill -KILL -f -- "$work/" 2> /dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

# session_gone <session id>: true once no process is left in that session, false if one still is
# after 20 s.
session_gone() {
  local tries=0
  while pgrep -s "$1" > /dev/null; do
    tries=$((tries + 1))
    if ((tries > 400)); then
      echo false
      return
    fi
    sleep 0.05
  done
  echo true
}

# appears <file>: true once the file exists, false if it does not within 20 s.
appears() {
  local tries=0
  until [[ -e $1 ]]; do
    tries=$((tries + 1))
    if ((tries > 400)); then
      echo false
      return
    fi
    sleep 0.05
  done
  echo true
}

# fresh <case>: points the pool at a directory of the case's own.
fresh() {
  export HARNESS_POOL_DIR="$work/$1/pool"
  mkdir -p "$work/$1"
  m="$work/$1"
}

# run_wait: sets out (the wait's stdout and stderr) and status.
run_wait() {
  status=0
  out=$(timeout 60 "$pool" wait 2>&1) || status=$?
}

echo "case: two harnesses run at once, and a start returns while its harness still runs"
fresh beside
export HARNESS_POOL_SLOTS=2
# Each harness says it started and then waits for the other's start: they both end only if they
# ran at the same time. The case releases neither; a pool that ran a harness inside its start
# would leave that start waiting forever, so each start is bounded and a start cut off by its
# bound fails the case.
start_status=0
timeout 20 "$pool" start one bash -c "touch '$m/one'; until [[ -e '$m/two' ]]; do sleep 0.05; done; echo one saw two" || start_status=$?
check "the first start returned while its harness still waited" "$(is "$start_status" 0)"
timeout 20 "$pool" start two bash -c "touch '$m/two'; until [[ -e '$m/one' ]]; do sleep 0.05; done; echo two saw one" || start_status=$?
check "and so did the second" "$(is "$start_status" 0)"
check "the first harness started" "$(appears "$m/one")"
check "the second started while the first still waited for it" "$(appears "$m/two")"
run_wait
check "both ended, and the wait passes" "$(is "$status" 0)"
check "it prints the first's log in a group of its own name" "$(contains "$out" '^::group::one: exit 0, ')"
check "with the harness's own output" "$(contains "$out" '^one saw two$')"
check "and the second's after it" "$(contains "$out" '^::group::two: exit 0, ')"
check "and names the count" "$(contains "$out" '^harness-pool: 2 harnesses, 0 failed$')"

echo "case: no more harnesses run at once than the slots, a start returns at once, and they start in the order queued"
fresh slots
export HARNESS_POOL_SLOTS=1
"$pool" start held bash -c "touch '$m/held-started'; until [[ -e '$m/release' ]]; do sleep 0.05; done"
check "the first harness started" "$(appears "$m/held-started")"
# The second and third wait for the one slot; their starts must return all the same, so each is
# bounded and a start cut off by its bound fails the case. The second looks for its slot every
# 2 s and the third every 50 ms, so a pool that let the first to look take a free slot would start
# the third first.
start_status=0
HARNESS_POOL_POLL=2 timeout 20 "$pool" start second bash -c "touch '$m/second-started'; sleep 0.5; touch '$m/second-ended'" || start_status=$?
check "a start returns while its harness waits for the one slot" "$(is "$start_status" 0)"
HARNESS_POOL_POLL=0.05 timeout 20 "$pool" start third bash -c "if [[ -e '$m/second-ended' ]]; then touch '$m/third-after-second'; fi; touch '$m/third-started'" || start_status=$?
check "and so does the next" "$(is "$start_status" 0)"
# Long enough for a pool that ignored the slot to have started a harness.
sleep 1
check "neither has started while the first holds the one slot" "$(is "$([[ -e $m/second-started || -e $m/third-started ]] && echo ran || echo waiting)" waiting)"
touch "$m/release"
check "the second starts once the first ends" "$(appears "$m/second-started")"
check "the third starts too" "$(appears "$m/third-started")"
check "but only once the second, queued before it, had ended" "$(is "$([[ -e $m/third-after-second ]] && echo after || echo before)" after)"
run_wait
check "all three passed" "$(is "$status" 0)"
check "and the wait gives each its seconds waiting for a slot" "$(contains "$out" '^::group::third: exit 0, [0-9.]* s, [0-9.]* s of CPU, after [0-9.]* s waiting for a slot$')"

echo "case: a harness takes the starting step's environment and working directory"
fresh environment
export HARNESS_POOL_SLOTS=2
mkdir -p "$m/workdir"
# shellcheck disable=SC2016 # the harness, not this shell, expands $STEP_VALUE and $PWD
(cd "$m/workdir" && STEP_VALUE=from-the-step "$pool" start env bash -c 'echo "value=$STEP_VALUE dir=$PWD"')
run_wait
check "it passes" "$(is "$status" 0)"
check "it read the step's env" "$(contains "$out" '^value=from-the-step ')"
check "in the step's directory" "$(contains "$out" "dir=$m/workdir\$")"

echo "case: a harness's CPU seconds are its own: a loop's are many, a sleep's next to none"
fresh cpu
export HARNESS_POOL_SLOTS=1
# shellcheck disable=SC2016 # the harness, not this shell, expands the loop
"$pool" start busy bash -c 'i=0; while ((i < 1000000)); do i=$((i + 1)); done'
# Queued behind the busy one, so its recorder polls for a slot all the while: none of that is its CPU.
"$pool" start idle bash -c 'sleep 2'
run_wait
check "both pass" "$(is "$status" 0)"
busy_cpu=$(sed -n 's/^::group::busy: exit 0, [0-9.]* s, \([0-9.]*\) s of CPU, .*/\1/p' <<< "$out")
idle_cpu=$(sed -n 's/^::group::idle: exit 0, [0-9.]* s, \([0-9.]*\) s of CPU, .*/\1/p' <<< "$out")
check "the loop used more than 0.3 s of CPU" "$(is "$(awk -v c="${busy_cpu:-0}" 'BEGIN { print (c > 0.3) ? "yes" : "no: " c }')" yes)"
check "the sleep, queued behind it, less than 0.3 s" "$(is "$(awk -v c="${idle_cpu:-9}" 'BEGIN { print (c < 0.3) ? "yes" : "no: " c }')" yes)"
export HARNESS_POOL_SLOTS=2

echo "case: starts at the same moment each take an entry of their own"
fresh concurrent
export HARNESS_POOL_SLOTS=8
for i in 1 2 3 4 5 6 7 8; do
  "$pool" start "same-$i" bash -c "echo said by $i" > /dev/null &
done
wait
run_wait
check "the wait passes" "$(is "$status" 0)"
check "and judges eight harnesses" "$(contains "$out" '^harness-pool: 8 harnesses, 0 failed$')"
for i in 1 2 3 4 5 6 7 8; do
  check "harness $i has its own entry and log" "$(contains "$out" "^said by $i\$")"
done
export HARNESS_POOL_SLOTS=2

echo "case: a harness starts with SIGINT and SIGQUIT at their defaults, as a step's shell does"
fresh signals
# The step's shell queues it with `&` and no job control, which starts a command with both
# ignored; a bash harness could then not trap the INT it sends what it tests. Each harness traps
# the one signal and sends it to itself: the trap runs only if the signal reached it at its
# default. (This reads the behaviour, not /proc/self/status's SigIgn line, which gVisor leaves out.)
# shellcheck disable=SC2016 # the harness, not this shell, expands these
"$pool" start int bash -c 'trap "echo trapped INT; exit 0" INT; kill -INT $$; sleep 5; echo no INT trap ran; exit 1'
# shellcheck disable=SC2016
"$pool" start quit bash -c 'trap "echo trapped QUIT; exit 0" QUIT; kill -QUIT $$; sleep 5; echo no QUIT trap ran; exit 1'
run_wait
check "the harness's INT trap ran" "$(contains "$out" '^trapped INT$')"
check "and the other's QUIT trap" "$(contains "$out" '^trapped QUIT$')"
check "both pass" "$(is "$status" 0)"

echo "case: a harness's output reaches the step only through the wait"
fresh output
start_out=$("$pool" start talker bash -c 'echo said-by-the-harness; echo on-stderr >&2' 2>&1)
check "the start says what it queued and nothing of the harness" "$(is "$start_out" 'harness-pool: queued talker (000)')"
run_wait
check "the wait prints the harness's standard output" "$(contains "$out" '^said-by-the-harness$')"
check "and its standard error" "$(contains "$out" '^on-stderr$')"

echo "case: each line of a harness's log is stamped with the time it was written, and the record is whole"
fresh lines
# A second between the lines: a record that gave them all one time, as a log read only at the
# harness's end would, fails the two gap checks. A slow machine lengthens the gaps.
"$pool" start three-lines bash -c 'echo first; sleep 1; echo second; sleep 1; echo third'
run_wait
check "it passes" "$(is "$status" 0)"
lines="$HARNESS_POOL_DIR/000.lines"
check "there is one record per line" "$(is "$(wc -l < "$lines")" 3)"
check "each record carries the line's own text, in order" "$(is "$(cut -f2- < "$lines" | paste -sd' ')" 'first second third')"
gap_one=$(awk -F'\t' 'NR == 1 { a = $1 } NR == 2 { printf "%.3f", $1 - a }' "$lines")
gap_two=$(awk -F'\t' 'NR == 2 { a = $1 } NR == 3 { printf "%.3f", $1 - a }' "$lines")
check "the second line's time is a sleep after the first's" "$(is "$(awk -v g="$gap_one" 'BEGIN { print (g >= 0.5) ? "yes" : "no: " g }')" yes)"
check "and the third's a sleep after the second's" "$(is "$(awk -v g="$gap_two" 'BEGIN { print (g >= 0.5) ? "yes" : "no: " g }')" yes)"
first_time=$(awk -F'\t' 'NR == 1 { printf "%.6f", $1 }' "$lines")
check "the first line's time is after the harness's own start" "$(is "$(awk -v t="$first_time" -v s="$(< "$HARNESS_POOL_DIR/000.started")" 'BEGIN { print (t >= s) ? "yes" : "no: " t " before " s }')" yes)"
check "the stamper counted the log's own bytes" "$(is "$(< "$HARNESS_POOL_DIR/000.lines.end")" "$(stat -c %s "$HARNESS_POOL_DIR/000.log")")"
check "and the recorder wrote that size beside the count, which is how a reader knows the record is whole" "$(is "$(< "$HARNESS_POOL_DIR/000.logbytes")" "$(< "$HARNESS_POOL_DIR/000.lines.end")")"

echo "case: the drain catches a last line the harness left without a newline"
fresh drain
"$pool" start unterminated bash -c "printf 'ended\nwith no newline'"
run_wait
check "it passes" "$(is "$status" 0)"
check "the last line is stamped too" "$(contains "$(cut -f2- < "$HARNESS_POOL_DIR/000.lines")" '^with no newline$')"
check "and the count still answers the log's size" "$(is "$(< "$HARNESS_POOL_DIR/000.lines.end")" "$(stat -c %s "$HARNESS_POOL_DIR/000.log")")"

echo "case: a Python harness's lines are stamped when it printed them, not when it ended"
fresh python-lines
# Without PYTHONUNBUFFERED a Python program writing to a file holds its lines until it ends, and
# both would carry that one time. The pool exports it to every harness for this reason. The start
# runs without it, so that a value the machine running this test already exports cannot stand in
# for the pool's own.
env -u PYTHONUNBUFFERED "$pool" start printer python3 -c $'import time\nprint("printed first")\ntime.sleep(1)\nprint("printed second")'
run_wait
check "it passes" "$(is "$status" 0)"
check "both lines are stamped" "$(is "$(wc -l < "$HARNESS_POOL_DIR/000.lines")" 2)"
python_gap=$(awk -F'\t' 'NR == 1 { a = $1 } NR == 2 { printf "%.3f", $1 - a }' "$HARNESS_POOL_DIR/000.lines")
check "the second line's time is a sleep after the first's" "$(is "$(awk -v g="$python_gap" 'BEGIN { print (g >= 0.5) ? "yes" : "no: " g }')" yes)"

echo "case: a stamper that cannot run changes no verdict, and leaves a record that says it is incomplete"
fresh stamper-fails
printf '#!/usr/bin/env bash\nexit 3\n' > "$m/stamper-that-fails"
chmod +x "$m/stamper-that-fails"
HARNESS_POOL_LINE_TIMES="$m/stamper-that-fails" "$pool" start unstamped bash -c 'echo said all the same; exit 0'
run_wait
check "the harness passes as ever" "$(is "$status" 0)"
check "its log is printed as ever" "$(contains "$out" '^said all the same$')"
check "and the wait judges it" "$(contains "$out" '^harness-pool: 1 harnesses, 0 failed$')"
check "the entry has its status" "$(is "$(< "$HARNESS_POOL_DIR/000.exit")" 0)"
check "and no count of stamped bytes, so a reader calls the record incomplete rather than whole" "$(is "$([[ -e $HARNESS_POOL_DIR/000.lines.end ]] && echo whole || echo incomplete)" incomplete)"

echo "case: a last line without a newline carries the time it was read, not the harness's end"
fresh unterminated-time
# The harness prints that line and then waits: a stamper that stamped it when it drained would
# give it the harness's end rather than its own moment.
"$pool" start prints-then-waits bash -c "printf 'ended\nwith no newline'; sleep 3"
run_wait
check "it passes" "$(is "$status" 0)"
last_time=$(awk -F'\t' 'END { printf "%.6f", $1 }' "$HARNESS_POOL_DIR/000.lines")
read -r last_wall _ < "$HARNESS_POOL_DIR/000.seconds"
ended_at=$(awk -v s="$(< "$HARNESS_POOL_DIR/000.started")" -v w="$last_wall" 'BEGIN { printf "%.6f", s + w }')
check "its time is a second or more before the harness ended" "$(is "$(awk -v l="$last_time" -v e="$ended_at" 'BEGIN { print (e - l >= 1) ? "yes" : "no: " (e - l) }')" yes)"

echo "case: a log rewritten behind the stamper leaves no whole record"
fresh rewritten
# The harness truncates its own log and writes a line of the same length over it: the two byte
# counts would agree, so the record would read as the log and hold something else. The stamper
# compares what it stamped with the log itself before it writes its count.
"$pool" start rewriter bash -c "echo 'PASS: old'; sleep 0.5; : > '$HARNESS_POOL_DIR/000.log'; printf 'PASS: new\n' > '$HARNESS_POOL_DIR/000.log'"
run_wait
check "the harness passes" "$(is "$status" 0)"
check "and the record says it is no whole one" "$(is "$([[ -e $HARNESS_POOL_DIR/000.lines.end ]] && echo whole || echo incomplete)" incomplete)"

echo "case: a harness whose log grows after it ended leaves a record that says it is a part"
fresh late-writer
# A process the harness left behind writes to the log after the stamper read it to the end: the
# size the recorder wrote is then not the log's, and the wait drops it, so a reader calls the
# record a part rather than the whole log.
"$pool" start leaves-a-writer bash -c "(sleep 1; echo 'written after the harness ended'; touch '$m/late-written') & echo 'PASS: first'"
check "the harness ended" "$(appears "$HARNESS_POOL_DIR/000.exit")"
check "and the process it left behind wrote to its log" "$(appears "$m/late-written")"
run_wait
check "the wait passes" "$(is "$status" 0)"
check "what the stamper read is kept" "$(is "$([[ -e $HARNESS_POOL_DIR/000.lines.end ]] && echo kept || echo none)" kept)"
check "and the log's size is dropped, so a reader calls the record a part" "$(is "$([[ -e $HARNESS_POOL_DIR/000.logbytes ]] && echo whole || echo "a part")" "a part")"

echo "case: a stamper that is not reading when the harness would start is ended, and stamps nothing"
fresh slow-stamper
# A stamper this late would stamp every line the harness had already written with the time of its
# own first read - one wrong time for many lines, which reads as a true one.
printf '#!/usr/bin/env bash\nsleep 30\nexec "%s" "$@"\n' "$script_dir/harness_line_times.py" > "$m/slow-stamper"
chmod +x "$m/slow-stamper"
HARNESS_POOL_LINE_TIMES="$m/slow-stamper" "$pool" start late bash -c 'echo said all the same'
run_wait
check "the harness passes as ever" "$(is "$status" 0)"
check "its log is printed as ever" "$(contains "$out" '^said all the same$')"
check "nothing was stamped" "$(is "$([[ -e $HARNESS_POOL_DIR/000.lines ]] && echo stamped || echo none)" none)"
check "and the record says it is no whole one" "$(is "$([[ -e $HARNESS_POOL_DIR/000.lines.end ]] && echo whole || echo incomplete)" incomplete)"

echo "case: a record file the recorder cannot write changes no verdict"
fresh record-write-fails
"$pool" start held-then-passes bash -c "touch '$m/held-started'; until [[ -e '$m/release' ]]; do sleep 0.05; done; echo said all the same"
check "the harness started" "$(appears "$m/held-started")"
# A link to a path that cannot exist where the recorder will write the stop file: the recorder's
# write of it fails, and the stamper, which looks for that file, finds none. Every write the
# record needs is made so that a failure to write it cannot end the recorder, which would leave
# the harness with no status and fail the wait over a harness that passed. The stamper, never
# asked to stop, is killed when the drain's 30 s are out.
ln -s "$m/absent/stop" "$HARNESS_POOL_DIR/000.lines.stop"
touch "$m/release"
run_wait
check "the harness still passes" "$(is "$status" 0)"
check "with its own status" "$(is "$(cat "$HARNESS_POOL_DIR/000.exit" 2> /dev/null || echo none)" 0)"
check "and its log" "$(contains "$out" '^said all the same$')"
check "the stop file was not written, so the stamper left no count of what it read" "$(is "$([[ -e $HARNESS_POOL_DIR/000.lines.end ]] && echo whole || echo incomplete)" incomplete)"

echo "case: a stamper's own seconds are never the harness's"
fresh stamper-cpu
# A stamper that burns CPU and ends before the harness does: the harness waits for its end and
# then sleeps half a second, by which time the recorder has reaped the stamper, so a `times` read
# by the recorder itself would count the stamper's seconds. The recorder runs the harness in a
# subshell and reads `times` there, and a subshell starts with no children's seconds, so the
# harness's CPU is its own whatever the recorder started beside it. The loop is the CPU case's
# million iterations, which that case requires to cost more than 0.3 s.
# shellcheck disable=SC2016 # the stamper, not this shell, expands these
printf '#!/usr/bin/env bash\n: > "$2"\ni=0\nwhile ((i < 1000000)); do i=$((i + 1)); done\ntouch %q\n' "$m/stamper-ended" > "$m/burning-stamper"
chmod +x "$m/burning-stamper"
HARNESS_POOL_LINE_TIMES="$m/burning-stamper" "$pool" start idle-harness bash -c "tries=0; until [[ -e '$m/stamper-ended' ]]; do ((tries++ < 200)) || exit 1; sleep 0.1; done; sleep 0.5"
run_wait
check "it passes, having waited for the stamper's end" "$(is "$status" 0)"
stamper_cpu=$(sed -n 's/^::group::idle-harness: exit 0, [0-9.]* s, \([0-9.]*\) s of CPU, .*/\1/p' <<< "$out")
check "the sleeping harness used next to no CPU" "$(is "$(awk -v c="${stamper_cpu:-9}" 'BEGIN { print (c < 0.3) ? "yes" : "no: " c }')" yes)"

echo "case: a CPU that cannot be read is written as unknown, never as none"
fresh cpu-unknown
export GITHUB_STEP_SUMMARY="$m/summary"
"$pool" start uncounted bash -c "touch '$m/uncounted-started'; until [[ -e '$m/release' ]]; do sleep 0.05; done"
check "the harness started" "$(appears "$m/uncounted-started")"
# A link to a path that cannot exist where the recorder will write the harness's `times`: that
# write fails, and the harness's CPU is then not known. A 0.0 in its place would be a wrong
# number that reads as a true one.
ln -s "$m/absent/times" "$HARNESS_POOL_DIR/000.times"
touch "$m/release"
run_wait
check "the harness still passes" "$(is "$status" 0)"
check "the wait gives its CPU as unknown" "$(contains "$out" '^::group::uncounted: exit 0, [0-9.]* s, ? s of CPU, after [0-9.]* s waiting for a slot$')"
check "and so does the step summary" "$(contains "$(cat "$m/summary")" '^| uncounted | [0-9.]* | ? | [0-9.]* | pass |$')"
unset GITHUB_STEP_SUMMARY

echo "case: a stamper that does not stop keeps no harness out of the slot its harness had"
fresh drain-slot
export HARNESS_POOL_SLOTS=1
# A stamper that reads nothing and never ends: the recorder asks it to stop once its harness has
# ended, and kills it only 30 s later. The harness's slot is free from the harness's end all the
# same, so the next harness runs while that recorder waits; the wait still waits for the first's
# status, which the recorder writes after the drain. The case kills the stamper itself once it
# has seen all that, so as not to spend the 30 s.
# shellcheck disable=SC2016 # the stamper, not this shell, expands these
printf '#!/usr/bin/env bash\necho "$$" > %q\n: > "$2"\nwhile :; do sleep 0.1; done\n' "$m/stamper.pid" > "$m/hung-stamper"
chmod +x "$m/hung-stamper"
HARNESS_POOL_LINE_TIMES="$m/hung-stamper" "$pool" start undrained bash -c 'echo said and ended'
"$pool" start next bash -c "if [[ -e '$HARNESS_POOL_DIR/000.exit' ]]; then touch '$m/after-status'; fi; touch '$m/next-started'"
check "the next harness starts while the first's recorder still drains its stamper" "$(appears "$m/next-started")"
status_now=0
out_now=$(timeout 60 "$pool" wait --now 2>&1) || status_now=$?
check "a wait stopped then names the first as ended, its status not yet written" "$(contains "$out_now" '^::error title=undrained::ended after [0-9.]* s, but its recorder had not yet written its status when the wait was stopped; ')"
"$pool" wait > "$m/wait.out" 2>&1 &
waiter=$!
# Long enough for a wait that did not wait for the first's status to have judged it.
sleep 1
check "a wait started while the first's recorder drains waits for its status" "$(is "$(kill -0 "$waiter" 2> /dev/null && echo waiting || echo ended)" waiting)"
kill -KILL "$(< "$m/stamper.pid")" 2> /dev/null || true
status=0
wait "$waiter" || status=$?
out=$(< "$m/wait.out")
check "and once the drain is over it passes" "$(is "$status" 0)"
check "judging the first by its own status" "$(contains "$out" '^::group::undrained: exit 0, ')"
check "the next harness had started before that status was written" "$(is "$([[ -e $m/after-status ]] && echo after || echo before)" before)"
export HARNESS_POOL_SLOTS=2

echo "case: a failing harness fails the wait, which names it, and the others still report"
fresh failure
export GITHUB_STEP_SUMMARY="$m/summary" GITHUB_OUTPUT="$m/step-output"
# The harness writes to the runner's per-step files as a step would; the queuing step has ended
# by then, so its writes go to its own entry and the summary reaches the wait's.
# shellcheck disable=SC2016 # the harness, not this shell, expands the variables
"$pool" start passes bash -c 'echo "summary line of passes" >> "$GITHUB_STEP_SUMMARY"; echo "value=1" >> "$GITHUB_OUTPUT"'
"$pool" start fails bash -c 'echo why it failed; exit 3'
run_wait
check "the wait fails" "$(is "$status" 1)"
check "it names the harness, its status and where its log is" "$(contains "$out" '^::error title=fails::exited 3 after [0-9.]* s; its log is in the group of that name above$')"
check "and prints that log" "$(contains "$out" '^why it failed$')"
check "the passing harness is still reported" "$(contains "$out" '^::group::passes: exit 0, ')"
check "and the count names the failure" "$(contains "$out" '^harness-pool: 2 harnesses, 1 failed$')"
summary_text=$(cat "$m/summary")
check "the summary has a row per harness" "$(contains "$summary_text" '^| fails | [0-9.]* | [0-9.]* | [0-9.]* | fail (3) |$')"
check "the passing one too" "$(contains "$summary_text" '^| passes | [0-9.]* | [0-9.]* | [0-9.]* | pass |$')"
check "what a harness wrote to its step summary reaches the wait's, under its name" "$(contains "$summary_text" '^### passes$')"
check "with its own text" "$(contains "$summary_text" '^summary line of passes$')"
check "and its output write does not reach the queuing step's file" "$(is "$([[ -e $m/step-output ]] && echo written || echo untouched)" untouched)"
unset GITHUB_STEP_SUMMARY GITHUB_OUTPUT

echo "case: a harness that signals its own process group is judged by its status"
fresh group
"$pool" start group-kill bash -c 'sleep 30 & kill -TERM 0; wait'
run_wait
check "the wait fails" "$(is "$status" 1)"
check "with the status the signal gave the harness, not with none" "$(contains "$out" '^::group::group-kill: exit 143, ')"

echo "case: a recorder killed before the harness ended is a failure, the wait does not hang, and it ends the harness"
fresh killed
"$pool" start orphan bash -c "touch '$m/orphan-started'; sleep 300"
check "the harness started" "$(appears "$m/orphan-started")"
check "the harness published its session" "$(appears "$HARNESS_POOL_DIR/000.session")"
read -r recorder _ < "$HARNESS_POOL_DIR/000.pid"
kill -KILL "$recorder"
run_wait
check "the wait fails rather than waiting out the harness" "$(is "$status" 1)"
check "and says the harness left no status" "$(contains "$out" '^::error title=orphan::exited none: its recorder ended without writing one after ? s')"
check "and the harness's session, left running by its recorder, is ended" "$(session_gone "$(< "$HARNESS_POOL_DIR/000.session")")"

echo "case: the harness of a recorder killed mid-run is ended before its slot goes to the next"
fresh orphan-slot
export HARNESS_POOL_SLOTS=1
"$pool" start stray bash -c "touch '$m/stray-started'; until [[ -e '$m/never' ]]; do sleep 0.05; done"
check "the first harness started" "$(appears "$m/stray-started")"
check "it published its session" "$(appears "$HARNESS_POOL_DIR/000.session")"
stray_session=$(< "$HARNESS_POOL_DIR/000.session")
# The next harness reads, as it starts, whether any process of the first's session is left.
"$pool" start next bash -c "if pgrep -s '$stray_session' > /dev/null; then touch '$m/overlap'; fi; touch '$m/next-started'"
read -r recorder _ < "$HARNESS_POOL_DIR/000.pid"
kill -KILL "$recorder"
check "the next harness starts in the freed slot" "$(appears "$m/next-started")"
check "with no process of the first left running beside it" "$(is "$([[ -e $m/overlap ]] && echo overlap || echo alone)" alone)"
run_wait
check "the wait fails, naming the lost one" "$(contains "$out" '^::error title=stray::exited none: its recorder ended ')"
export HARNESS_POOL_SLOTS=2

echo "case: a recorder killed while its harness waits for a slot frees the queue"
fresh killed-queued
export HARNESS_POOL_SLOTS=1
"$pool" start holder bash -c "touch '$m/holder-started'; until [[ -e '$m/release' ]]; do sleep 0.05; done"
check "the first holds the one slot" "$(appears "$m/holder-started")"
"$pool" start doomed bash -c "touch '$m/doomed-started'"
"$pool" start after bash -c "touch '$m/after-started'"
read -r recorder _ < "$HARNESS_POOL_DIR/001.pid"
kill -KILL "$recorder"
touch "$m/release"
check "the harness queued after the killed one still runs" "$(appears "$m/after-started")"
run_wait
check "the wait fails" "$(is "$status" 1)"
check "naming the killed one" "$(contains "$out" '^::error title=doomed::exited none: its recorder ended ')"
check "which never ran" "$(is "$([[ -e $m/doomed-started ]] && echo ran || echo never)" never)"
export HARNESS_POOL_SLOTS=2

echo "case: a harness whose entry was claimed as lost before it began runs nothing"
fresh claimed
export HARNESS_POOL_SLOTS=1
"$pool" start gate bash -c "touch '$m/gate-started'; until [[ -e '$m/release' ]]; do sleep 0.05; done"
check "the first holds the one slot" "$(appears "$m/gate-started")"
"$pool" start claimed bash -c "touch '$m/claimed-ran'"
# As a count does for an entry whose recorder it found gone, while the harness had not yet
# published its session: the harness must then find the claim and not run.
mkdir "$HARNESS_POOL_DIR/001.lost"
touch "$m/release"
check "the claimed entry ended" "$(appears "$HARNESS_POOL_DIR/001.exit")"
check "without running its command" "$(is "$([[ -e $m/claimed-ran ]] && echo ran || echo never)" never)"
check "with the status the harness gives a claimed entry" "$(is "$(< "$HARNESS_POOL_DIR/001.exit")" 125)"
run_wait
export HARNESS_POOL_SLOTS=2

echo "case: a running harness whose entry another caller claimed as lost and then left is still ended"
fresh abandoned
export HARNESS_POOL_SLOTS=1
"$pool" start left bash -c "touch '$m/left-started'; until [[ -e '$m/never' ]]; do sleep 0.05; done"
check "the first harness started" "$(appears "$m/left-started")"
check "it published its session" "$(appears "$HARNESS_POOL_DIR/000.session")"
# A claim whose maker died before it ended the session: the claim is there, and nothing killed.
mkdir "$HARNESS_POOL_DIR/000.lost"
"$pool" start after-left bash -c "touch '$m/after-left-started'"
read -r recorder _ < "$HARNESS_POOL_DIR/000.pid"
kill -KILL "$recorder"
check "the next harness still gets the slot" "$(appears "$m/after-left-started")"
check "and the first's session is ended" "$(session_gone "$(< "$HARNESS_POOL_DIR/000.session")")"
run_wait
export HARNESS_POOL_SLOTS=2

echo "case: an entry's pid that names another process is not its recorder"
fresh reused
"$pool" start ended-early bash -c 'exit 0'
check "the harness ended" "$(appears "$HARNESS_POOL_DIR/000.exit")"
# A recorder that died without its status, and whose pid a process alive now holds: this test's
# own pid, with a start time that is not the recorder's.
rm "$HARNESS_POOL_DIR/000.exit"
echo "$$ 1" > "$HARNESS_POOL_DIR/000.pid"
run_wait
check "the wait does not wait on the process that holds the pid now" "$(is "$status" 1)"
check "and judges the entry a failure" "$(contains "$out" '^::error title=ended-early::exited none: its recorder ended ')"

echo "case: a wait over no harness judges nothing, and says so"
fresh empty
run_wait
check "it exits 2" "$(is "$status" 2)"
check "and names the directory it found empty" "$(contains "$out" "^::error::harness-pool: no harness was queued under $HARNESS_POOL_DIR, so there is nothing to judge\$")"

echo "case: a slot count that is no positive whole number is refused"
fresh refused
status=0
out=$(HARNESS_POOL_SLOTS=0 "$pool" start never true 2>&1) || status=$?
check "it exits 2" "$(is "$status" 2)"
check "and names the value" "$(contains "$out" "^::error::harness-pool: HARNESS_POOL_SLOTS is '0', not a positive whole number\$")"
check "having started nothing" "$(is "$([[ -e $HARNESS_POOL_DIR ]] && echo started || echo none)" none)"

echo "case: a harness started with --minutes sees its deadline, its start plus the bound less 30 s"
fresh deadline
unset HARNESS_POOL_DEADLINE_MS
before=$(date +%s%3N)
# shellcheck disable=SC2016 # the harness, not this shell, expands the variable
"$pool" start --minutes 2 bounded bash -c 'echo "deadline=${HARNESS_POOL_DEADLINE_MS:-unset}"'
# shellcheck disable=SC2016
"$pool" start unbounded bash -c 'echo "unbounded deadline=${HARNESS_POOL_DEADLINE_MS:-unset}"'
run_wait
after=$(date +%s%3N)
check "both pass" "$(is "$status" 0)"
deadline=$(sed -n 's/^deadline=//p' <<< "$out")
check "the value is a number" "$(is "$([[ $deadline =~ ^[0-9]+$ ]] && echo number || echo "'$deadline'")" number)"
started=$(< "$HARNESS_POOL_DIR/000.started")
frac=${started#*.}
start_ms=$((${started%.*} * 1000 + 10#${frac:0:3}))
check "it is the harness's own start, in epoch milliseconds, plus 120 s less 30 s" "$(is "$deadline" "$((start_ms + 90000))")"
in_range=false
if [[ $deadline =~ ^[0-9]+$ ]] && ((deadline >= before + 90000 && deadline <= after + 90000)); then
  in_range=true
fi
check "and that start lies between the clock before the start and after the wait" "$in_range"
check "a harness started without --minutes sees none" "$(contains "$out" '^unbounded deadline=unset$')"

echo "case: a harness past its --minutes bound is stopped, and the wait names the bound"
fresh bound
# A minute of one second: the bound is 2 s, and the harness would sleep 300.
HARNESS_POOL_SECONDS_PER_MINUTE=1 "$pool" start --minutes 2 overrun bash -c "touch '$m/overrun-started'; sleep 300; touch '$m/overrun-finished'"
# A harness that exits 124 itself, inside its bound, is a plain failure, not a stop.
HARNESS_POOL_SECONDS_PER_MINUTE=1 "$pool" start --minutes 30 says-124 bash -c 'exit 124'
check "the harness started" "$(appears "$m/overrun-started")"
run_wait
check "the wait fails" "$(is "$status" 1)"
check "and says the harness ran past its bound in the minutes it was given" "$(contains "$out" '^::error title=overrun::ran past its bound of 2 minutes and was stopped (exit 124); its log is in the group of that name above$')"
check "the harness was stopped, not let run to its end" "$(is "$([[ -e $m/overrun-finished ]] && echo finished || echo stopped)" stopped)"
check "a harness that exited 124 inside its bound is named by its status" "$(contains "$out" '^::error title=says-124::exited 124 after [0-9.]* s; ')"

echo "case: a --minutes that is no positive whole number is refused"
fresh minutes
status=0
out=$("$pool" start --minutes soon never true 2>&1) || status=$?
check "it exits 2" "$(is "$status" 2)"
check "and names the value" "$(contains "$out" "^::error::harness-pool: --minutes is 'soon', not a positive whole number\$")"
check "having started nothing" "$(is "$([[ -e $HARNESS_POOL_DIR ]] && echo started || echo none)" none)"

echo "case: --allow-none lets a wait over no harness pass, and judges a started one as ever"
fresh allow-none
status=0
out=$(timeout 60 "$pool" wait --allow-none 2>&1) || status=$?
check "over none it exits 0" "$(is "$status" 0)"
check "and says it judged nothing" "$(contains "$out" "^harness-pool: no harness was queued under $HARNESS_POOL_DIR, and --allow-none: nothing to judge\$")"
# What the uploaded record says of a job that queued no harness: 0 judged, which a reader tells
# from a record that was lost.
check "and writes that it judged none" "$(is "$(cat "$HARNESS_POOL_DIR/judged" 2> /dev/null || echo none written)" 0)"
"$pool" start fails bash -c 'exit 5'
status=0
out=$(timeout 60 "$pool" wait --allow-none 2>&1) || status=$?
check "over a failing harness it still fails" "$(is "$status" 1)"
check "naming it" "$(contains "$out" '^::error title=fails::exited 5 after ')"
check "and writes how many harnesses it judged" "$(is "$(cat "$HARNESS_POOL_DIR/judged" 2> /dev/null || echo none written)" 1)"
status=0
out=$(timeout 60 "$pool" wait --none 2>&1) || status=$?
check "a wait refuses any other argument" "$(is "$status" 2)"

echo "case: a wait stopped as the runner cancels a step names each harness still running and prints its log so far"
fresh stopped
"$pool" start finished bash -c 'echo said before the stop'
"$pool" start hung bash -c "echo before the hang; touch '$m/hung-started'; until [[ -e '$m/release-hung' ]]; do sleep 0.05; done"
check "the hanging harness started" "$(appears "$m/hung-started")"
check "the other one ended" "$(appears "$HARNESS_POOL_DIR/000.exit")"
# One slot for this one, held by the hanging harness: it is still queued when the wait is stopped.
HARNESS_POOL_SLOTS=1 "$pool" start behind bash -c "touch '$m/behind-started'"
# In a process group of its own, so the signals below reach the wait's group: the poll's sleep and
# every process the report starts take them too. A background job of this script is no group
# leader, so setsid runs the wait in its own pid, which is the group's id.
# This script, like the step's shell, has no job control, so a command it starts with `&` would
# start with SIGINT ignored, which a step's own process does not: env puts it back.
# start_waiter <file>: starts a wait into that file, sets `waiter`, and checks that it says it is
# waiting, which it does once its trap is set; a signal before that would end it unheard.
start_waiter() {
  env --default-signal=INT setsid "$pool" wait > "$1" 2>&1 &
  waiter=$!
  local waiting=false
  for _ in $(seq 400); do
    if grep -q '^harness-pool: waiting for ' "$1"; then
      waiting=true
      break
    fi
    sleep 0.05
  done
  check "the wait says it is waiting" "$waiting"
}
# A cancel's INT alone first.
start_waiter "$m/int.out"
kill -INT -- "-$waiter"
status=0
wait "$waiter" || status=$?
out=$(cat "$m/int.out")
check "an INT alone stops the wait, which fails" "$(is "$status" 1)"
check "naming the harness still running" "$(contains "$out" '^::error title=hung::was still running after [0-9.]* s when the wait was stopped; ')"
# Then an INT followed by TERMs, as the runner sends one 7.5 s after the INT, here one every 10 ms
# until the wait has ended, so some land while it writes its report.
start_waiter "$m/wait.out"
kill -INT -- "-$waiter"
while kill -0 "$waiter" 2> /dev/null; do
  kill -TERM -- "-$waiter" 2> /dev/null || true
  sleep 0.01
done
status=0
wait "$waiter" || status=$?
out=$(cat "$m/wait.out")
# A job cancelled before its wait step began: --now judges at once.
status_now=0
out_now=$(timeout 60 "$pool" wait --now 2>&1) || status_now=$?
# What the cancel leaves of the running harness's record, read before it is released: every
# record is flushed as it is read, so the line it printed before it hung is already stamped,
# while the entry has no count of stamped bytes, which is how a reader knows it is a part.
hung_stamped=$(cut -f2- < "$HARNESS_POOL_DIR/001.lines")
hung_record=$([[ -e $HARNESS_POOL_DIR/001.lines.end ]] && echo whole || echo a part)
touch "$m/release-hung"
check "the cancelled wait leaves what the running harness had printed, stamped" "$(contains "$hung_stamped" '^before the hang$')"
check "and leaves that record as a part, not as a whole one" "$(is "$hung_record" "a part")"
check "--now fails at once" "$(is "$status_now" 1)"
check "and says the job was cancelled" "$(contains "$out_now" '^harness-pool: the job was cancelled; judging the 3 harnesses queued as they are now$')"
check "naming the harness still running" "$(contains "$out_now" '^::error title=hung::was still running after [0-9.]* s when the wait was stopped; ')"
check "the stopped wait fails" "$(is "$status" 1)"
check "it names the harness still running, with its seconds" "$(contains "$out" '^::group::hung: still running after [0-9.]* s when the wait was stopped; its log so far$')"
check "and prints its log so far" "$(contains "$out" '^before the hang$')"
check "and an error naming it" "$(contains "$out" '^::error title=hung::was still running after [0-9.]* s when the wait was stopped; ')"
check "it names the harness still waiting for a slot as never run" "$(contains "$out" '^::error title=behind::was still waiting for a slot when the wait was stopped, and never ran$')"
check "the harness that had ended is reported as ever" "$(contains "$out" '^::group::finished: exit 0, ')"
check "and the count names the two unfinished" "$(contains "$out" '^harness-pool: 3 harnesses, 0 failed, 2 unfinished when the wait was stopped$')"
check "the released harness ends" "$(appears "$HARNESS_POOL_DIR/001.exit")"
check "and the one behind it then runs" "$(appears "$m/behind-started")"

summary "the harness pool"
