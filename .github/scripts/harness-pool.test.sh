#!/usr/bin/env bash
# Tests for `harness-pool.sh`: harnesses started by one step run beside each other, never more at
# once than the slots and in the order queued, each in its own session with the starting step's
# environment and working directory and SIGINT at its default; starts at the same moment take
# entries of their own; a harness's CPU seconds are its own; a harness started with --minutes sees
# its deadline and is stopped at its bound; and the wait is the verdict on every one of them, a
# failure, a harness that signals its own process group, a recorder that was killed (before or
# after its harness started, or replaced by another process of its pid), and a wait stopped by an
# INT or a TERM included, and on none started.
#
# Every case drives the script CI runs, in a pool directory of its own. The harnesses are small
# bash commands that wait on marker files the case writes, and each wait for a file gives up after
# 20 s and fails its case by name rather than hanging. Three checks lean on a clock, each in the
# direction where a slow machine can only let a broken pool pass, never fail a working one: the
# slot case gives a pool that ignored the slot 1 s to start a harness it must not; the bound case
# shortens a minute to one second through HARNESS_POOL_SECONDS_PER_MINUTE and requires that a
# harness which would sleep 300 s is stopped; and the CPU case requires a loop of a million
# iterations to cost more than 0.3 s of CPU and a sleep to cost less.
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
"$pool" start fails bash -c 'exit 5'
status=0
out=$(timeout 60 "$pool" wait --allow-none 2>&1) || status=$?
check "over a failing harness it still fails" "$(is "$status" 1)"
check "naming it" "$(contains "$out" '^::error title=fails::exited 5 after ')"
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
touch "$m/release-hung"
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
