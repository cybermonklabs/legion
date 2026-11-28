#!/usr/bin/env bash
# harness-pool runs a job's independent test harnesses beside each other on the runner's cores,
# where one workflow step after another would run them one at a time.
#
# A job's steps run in series, and most of a shell harness's wall time is waiting: on a fake's
# answer, a poll, a process's end. So a job of twenty harness steps keeps three of a 4-vCPU
# runner's cores idle for most of its run. Here each harness step keeps its own name and its own
# `if:`, and only hands its command to the pool, which queues it and returns; a last step waits
# for every harness the job queued and is the job's verdict on them.
#
#   harness-pool.sh start [--minutes <n>] <name> <command> [<argument>...]
#     Queues the command and returns at once: the step passes whatever the harness does, and the
#     job's next steps, its other checks included, run while the harness waits and runs. In the
#     background the command starts once fewer than HARNESS_POOL_SLOTS harnesses are running
#     (default: the runner's processor count, nproc), in the order the steps queued them, so no
#     more run at once than there are cores and the first queued starts first. It runs in the
#     step's working directory and environment, in a process group of its own, with its standard
#     input from /dev/null and its standard output and error into a log of its own.
#     --minutes is the harness's own bound, what a step's timeout-minutes was for it: the command
#     runs under `timeout` for that long from its own start (exit 124, or 137 when it ignored the
#     TERM and took the KILL 30 s later; the wait names either as the bound), and sees
#     HARNESS_POOL_DEADLINE_MS, its start plus the bound less 30 s in epoch milliseconds, for a
#     harness whose waits give up at a deadline and name what they waited for.
#   harness-pool.sh wait [--allow-none] [--now]
#     Waits until every harness queued has ended and its recorder has written its status.
#     Prints each one's log in a group of its own, in the order they were queued, headed by its
#     name, its exit status, its wall and CPU seconds (`?` for seconds that could not be read)
#     and the seconds it waited for a slot; prints an ::error:: line naming each that failed;
#     writes one table row per harness to GITHUB_STEP_SUMMARY when that is set. Exits 1 when any
#     harness exited other than 0 or left no exit status. Exits 2 when no harness was queued,
#     since a wait that judged nothing must not read as a pass; --allow-none makes that 0, for a
#     job whose every harness step has an `if:` of its own, where a run whose change reached none
#     of them rightly starts none.
#     A TERM or INT stops the wait (the runner signals a step it cancels): it then prints the log
#     so far of each harness still running, names each as unfinished with its seconds, names each
#     that had ended while its recorder had not yet written its status, names each still waiting
#     for a slot, and exits 1, so a harness that hung is named in the job's log rather than lost
#     with the step. --now does the same at once, for a job cancelled before its
#     wait step began (a step of its own, `if: cancelled()`). The wait step runs the wait with
#     `exec` under `if: ${{ !cancelled() }}`, so a cancel's signal reaches the wait itself: the
#     runner signals a running step only when its condition no longer holds.
#
# Each harness inherits the starting step's environment, `env:` included, and its working
# directory, but for the runner's per-step files: the step that queued it has ended by the time
# it runs, so GITHUB_STEP_SUMMARY, GITHUB_OUTPUT, GITHUB_ENV, GITHUB_PATH and GITHUB_STATE name
# files of its own entry instead, and the wait adds what the harness wrote to its step summary to
# the wait's own. A harness cannot set an output, an environment variable or a path for a later
# step. The pool's state is a directory, HARNESS_POOL_DIR (default
# $RUNNER_TEMP/harness-pool), with one numbered entry per harness: its name, its log, its start
# once it has a slot, the end of its command, and, after that, its exit status and seconds. A
# harness whose process went away without writing its status is a failure, never a pass.
#
# Beside the log of each harness the pool records when each of its lines was written. GitHub
# stamps a job's log by the line, and a pooled harness's log reaches GitHub only when the wait
# prints it at the end, so every line of it carries the wait's time and each case's own time is
# lost. .github/scripts/harness_line_times.py, started by the recorder before the harness and
# reading the log beside it, writes `<epoch seconds with microseconds>\t<the line's bytes>` per
# line to `<entry>.lines`. Once the harness's command has returned, the recorder writes
# `<entry>.ended`, from which no count gives the entry a slot, and only then asks the stamper to
# stop: the stamper then reads the log to its end and writes the bytes it stamped to
# `<entry>.lines.end`, beside which the recorder writes the log's own size as `<entry>.logbytes`.
# So a queued harness takes the slot while the recorder drains a slow or hung stamper, and the
# wait still waits for the entry's status. A reader takes the record as whole only when the two
# sizes agree (.github/scripts/harness_case_times.py), and the recorder writes the entry's exit
# status last of all, so an entry with a status has a record that is complete or says it is not.
# The stamper reads the log and never the harness: the harness writes its output to the same file
# as before the record existed, and nothing of the stamper's stands between the harness and that
# file, so a stamper that fails to start, is killed or is cut off changes no harness's verdict and
# no wait's. (The one thing the recorder changes for the harness is PYTHONUNBUFFERED, below.) Such
# a stamper leaves the record incomplete instead, which the reader reports as UNKNOWN, and leaves
# its own error output in the entry's recorder log, which the wait prints in the harness's group.
# A job cancelled mid-run keeps whatever was stamped up to the cancel, since every record is
# flushed as it is read. Every wait writes `judged` in the pool's directory first of all, the
# number of entries it found and 0 for none, so that a reader can tell a job that queued no
# harness from one whose record was lost: both would otherwise be a job with no case in it.
# HARNESS_POOL_LINE_TIMES names the stamper (default: harness_line_times.py beside this
# script); it is a seam for harness-pool.test.sh, which points it at a stamper that fails to
# check that a failing stamper changes nothing. No workflow sets it.
#
# The recorder exports PYTHONUNBUFFERED=1 to the harness: a Python program writing to a file
# block-buffers its output, so without it a Python harness's lines would all reach the log when
# it ended and be stamped with that one time - a wrong time, which reads as a true one, where a
# missing record reads as missing. It changes no output and no status; it changes buffering, and
# with it the order in which a Python program's standard output and error reach the log, which is
# then the order it wrote them.
#
# The runner does not end a step's background processes when the step ends; it ends them at the
# job's end ("Cleaning up orphan processes"), so a harness outlives the step that started it and
# the job's timeout-minutes still bounds it.
#
# HARNESS_POOL_SECONDS_PER_MINUTE (default 60) is the length of one of --minutes' minutes in
# seconds. It is a seam for harness-pool.test.sh alone, which stops a harness at its bound in a
# few seconds rather than in minutes; no workflow sets it.
#
# Its tests are in harness-pool.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

dir=${HARNESS_POOL_DIR:-${RUNNER_TEMP:?RUNNER_TEMP or HARNESS_POOL_DIR must name the directory of the pool}/harness-pool}
slots=${HARNESS_POOL_SLOTS:-$(nproc)}
if ! [[ $slots =~ ^[1-9][0-9]*$ ]]; then
  echo "::error::harness-pool: HARNESS_POOL_SLOTS is '$slots', not a positive whole number" >&2
  exit 2
fi
seconds_per_minute=${HARNESS_POOL_SECONDS_PER_MINUTE:-60}
if ! [[ $seconds_per_minute =~ ^[1-9][0-9]*$ ]]; then
  echo "::error::harness-pool: HARNESS_POOL_SECONDS_PER_MINUTE is '$seconds_per_minute', not a positive whole number" >&2
  exit 2
fi
# How often a harness waiting for a slot, and the wait, look again. Not part of any verdict.
poll=${HARNESS_POOL_POLL:-0.5}
# The stamper the recorder starts beside each harness, on the harness's log (header above).
line_times=${HARNESS_POOL_LINE_TIMES:-$(dirname "${BASH_SOURCE[0]}")/harness_line_times.py}

# proc_start <pid>: sets `pstart` to the process's start time (field 22 of /proc/<pid>/stat), or
# fails when there is no such process. Forks nothing. A pid with its start time names one process:
# a pid alone could, once its process ended, name a later one.
proc_start() {
  local stat fields
  pstart=""
  read -r stat 2> /dev/null < "/proc/$1/stat" || return 1
  # The fields after the command's name, which is in parentheses and may hold spaces; field 22
  # is the 20th of them.
  read -ra fields <<< "${stat##*) }"
  pstart=${fields[19]:-}
  [[ -n $pstart ]]
}

# alive <base>: whether that entry's recorder is still to write its exit status. It has written
# it, or its recorder is gone without writing one (killed), which the wait judges a failure;
# without the second test a killed recorder would hold the wait until the job's timeout. An entry
# alive with its `.ended` is one whose harness has ended while its recorder drains the stamper
# (drain_stamper): it holds no slot, but the wait still waits for its status.
# The recorder is the pid the entry holds with the start time it had when it was queued.
alive() {
  local pid want
  [[ ! -e $1.exit ]] && read -r pid want < "$1.pid" && proc_start "$pid" && [[ $pstart == "$want" ]]
}

# lost_running <base>: for an entry whose recorder is gone without a status, true while any
# process of its harness's session is still there. It claims the entry as lost (`.lost`), ends
# its session, and once the session is empty marks the entry gone, after which it forks no more
# for it. The harness publishes its session itself and then looks for the claim before it runs
# (see run), so whichever of the two comes first, nothing the harness started in that session
# runs on past a claim: a harness on a core no slot counts would let one more run than the slots.
# A process the harness moved into a session of its own (setsid, as vm.test.sh's stand-ins do) is
# beyond this, as it is beyond a step's own end; the job's end still reaches it. Every caller
# kills, not the first alone, so a caller that dies after the claim leaves the kill to the next.
lost_running() {
  [[ -e $1.exit || -e $1.gone ]] && return 1
  mkdir "$1.lost" 2> /dev/null || true
  if [[ -e $1.session ]]; then
    local session
    session=$(< "$1.session")
    pkill -KILL -s "$session" 2> /dev/null || true
    if pgrep -s "$session" > /dev/null; then
      return 0
    fi
  fi
  : > "$1.gone"
  return 1
}

# count [<base>]: sets `running` to the number of harnesses that hold a slot (started and not
# ended, or lost with a process of their session still there), `draining` to those whose harness
# has ended and whose recorder has not yet written their status, `waiting` to those still queued
# for one, and `ahead` to those queued before <base> that have not started. A draining entry
# holds no slot, since what is left of it is the record, which may not keep a harness waiting.
# It forks nothing but for a lost entry, since every queued harness and the wait ask it twice a
# second.
count() {
  local entry other mine=${1:-}
  mine=${mine##*/}
  running=0 draining=0 waiting=0 ahead=0
  for entry in "$dir"/*.name; do
    [[ -e $entry ]] || continue
    other=${entry%.name}
    if ! alive "$other"; then
      if lost_running "$other"; then
        running=$((running + 1))
      fi
      continue
    fi
    if [[ -e $other.ended ]]; then
      draining=$((draining + 1))
    elif [[ -e $other.started ]]; then
      running=$((running + 1))
    else
      waiting=$((waiting + 1))
      if [[ -n $mine ]] && ((10#${other##*/} < 10#$mine)); then
        ahead=$((ahead + 1))
      fi
    fi
  done
}

# admit <base>: returns once that entry may take a slot: its start has written its name (and so
# its pid, which the counts read), no entry queued before it is still waiting, and fewer than the
# slots are running. An entry is admitted only once every one before it has written its start, so
# two never take the last slot together.
admit() {
  until [[ -e $1.name ]] && count "$1" && ((ahead == 0 && running < slots)); do
    sleep "$poll"
  done
}

start() {
  local minutes=""
  if [[ ${1:-} == --minutes ]]; then
    minutes=${2:-}
    shift 2 || true
    if ! [[ $minutes =~ ^[1-9][0-9]*$ ]]; then
      echo "::error::harness-pool: --minutes is '$minutes', not a positive whole number" >&2
      exit 2
    fi
  fi
  if (($# < 2)); then
    echo "::error::harness-pool: start takes a name and a command" >&2
    exit 2
  fi
  local name=$1
  shift
  mkdir -p "$dir"
  # One start at a time takes an entry: the lock is held from the count of entries to the name
  # that makes the entry count, so two starts at once never take the same number. The recorder
  # does not inherit it.
  local lock
  exec {lock}> "$dir/.lock"
  flock "$lock"
  local entries=("$dir"/*.name) id
  if [[ -e ${entries[0]} ]]; then
    id=$(printf '%03d' "${#entries[@]}")
  else
    id=000
  fi
  local base="$dir/$id"
  # setsid puts the recorder, and the harness it starts, in a session of its own, so the
  # harness's kills of its process group reach neither this step's shell nor another harness;
  # --wait keeps setsid's own pid, the one recorded below, alive until the recorder ends, whether
  # or not setsid had to fork. Every descriptor goes to a file: a background process holding the
  # step's output open would keep the step from ending. The recorder's own output, its errors,
  # goes to the entry's recorder log, which the wait prints when it holds anything.
  setsid --wait "${BASH_SOURCE[0]}" run "$base" "$minutes" "$@" > "$base.recorder.log" 2>&1 < /dev/null {lock}>&- &
  # The pid and its start time before the name: an entry is counted from its name, and read for
  # its pid. A recorder already gone has no start time, and its entry is judged by its status.
  proc_start "$!" || pstart=gone
  echo "$! $pstart" > "$base.pid"
  printf '%s\n' "$name" > "$base.name"
  exec {lock}>&-
  echo "harness-pool: queued $name ($id)"
}

# run <base> <minutes or empty> <command>...: the background half of start, in a session of its
# own. Waits for the entry's slot, then runs the command, in a session of its own again, into its
# log; once the command has returned it writes `.ended`, which gives up the slot (count), drains
# the stamper, and writes the harness's seconds and then its exit status, the status last and by
# a rename, so an entry with a status is complete. The command's own session keeps this recorder
# alive through a harness that signals its whole process group, so even that harness ends with a
# status rather than with none.
#
# The command starts with SIGINT and SIGQUIT at their defaults, as a step's own shell starts. This
# recorder was started with `&` by a shell without job control, which starts it with both ignored
# (POSIX), and a non-interactive bash cannot trap a signal it was started with ignored: a harness
# that interrupts what it tests with an INT would find the trap it tests doing nothing.
# `env --default-signal` puts them back for the command and all it starts.
#
# The command's session is published by the command's own first process, before it runs the
# command: it writes its pid, which setsid made the session's id, and then runs nothing if the
# entry was already claimed as lost (lost_running), so a recorder killed at any moment leaves no
# harness that the pool cannot see and end.
# shellcheck disable=SC2016 # the harness's first process, not this shell, expands these
publish_then_run='printf "%s\n" "$$" > "$1.session"; if [[ -e $1.lost ]]; then exit 125; fi; shift; exec "$@"'
run() {
  local base=$1 minutes=$2 queued started frac start_ms ended status=0 inner=0 cpu bound stamper
  shift 2
  queued=$(date +%s.%N)
  export GITHUB_STEP_SUMMARY="$base.step-summary" GITHUB_OUTPUT="$base.github-output" \
    GITHUB_ENV="$base.github-env" GITHUB_PATH="$base.github-path" GITHUB_STATE="$base.github-state"
  # A Python harness, and a Python program a harness runs, writes its lines to the log when it
  # prints them rather than a block at a time, so the stamper below gives each its own time.
  export PYTHONUNBUFFERED=1
  # The log and the stamper before the slot, and the harness only once the stamper is reading:
  # an interpreter takes a fifth of a second to start, and a line written before the stamper's
  # first read would carry that read's time rather than its own. An entry queued before this one
  # has written its start by the time this one is admitted (`admit`), so the start written below,
  # after the wait, still holds the slot accounting. The harness's own redirection truncates this
  # empty file and writes it as it always did. The stamper is given this recorder's pid and its
  # start time, so that it ends itself whenever the recorder is gone, however early that is.
  : > "$base.log"
  proc_start "$$" || pstart=0
  "$line_times" "$base.log" "$base.lines" "$base.lines.stop" "$base.lines.end" "$$" "$pstart" &
  stamper=$!
  admit "$base"
  if ! stamper_reading "$base" "$stamper"; then
    # It is not reading yet, so every line the harness wrote before its first read would carry
    # that read's time: one wrong time for many lines, which reads as a true one where a missing
    # record reads as missing. End it. The entry then has no record, which the reader reports as
    # UNKNOWN, and the harness runs as it always did.
    kill -KILL "$stamper" 2> /dev/null || true
    wait "$stamper" 2> /dev/null || true
    stamper=""
  fi
  started=$(date +%s.%N)
  echo "$started" > "$base.started"
  if [[ -n $minutes ]]; then
    bound=$((minutes * seconds_per_minute))
    # The minutes as given, for the wait's message, and the bound in seconds, for its judgement.
    echo "$minutes $bound" > "$base.bound"
    frac=${started#*.}
    start_ms=$((${started%.*} * 1000 + 10#${frac:0:3}))
  fi
  # The harness runs in a subshell of its own, which reads `times` for it: a shell starts with no
  # children's seconds of its own, so the second line of that `times` is the harness and
  # everything it started and nothing else - the stamper this recorder started beside it cannot
  # reach that number, whether or not it has ended. Beside the wall seconds it says how much of
  # the harness was waiting rather than computing. The subshell ends with the harness's own
  # status, and a `times` that cannot be written changes neither it nor the entry's.
  (
    if [[ -n $minutes ]]; then
      # timeout runs in the harness's session, so its signal at the bound reaches the harness and
      # every process it started; the kill 30 s later reaches one that ignored it.
      HARNESS_POOL_DEADLINE_MS=$((start_ms + (bound - 30) * 1000)) \
        env --default-signal=INT,QUIT setsid --wait bash -c "$publish_then_run" harness-pool "$base" \
        timeout --kill-after=30 "$bound" "$@" > "$base.log" 2>&1 < /dev/null || inner=$?
    else
      env --default-signal=INT,QUIT setsid --wait bash -c "$publish_then_run" harness-pool "$base" \
        "$@" > "$base.log" 2>&1 < /dev/null || inner=$?
    fi
    times > "$base.times" || true
    exit "$inner"
  ) || status=$?
  ended=$(date +%s.%N)
  # The harness has ended, so its slot is free for the next one queued while the stamper below
  # drains: the wait still waits for the status, which is written after the drain. A marker that
  # cannot be written only keeps the slot held until the status, as before the marker existed,
  # so its failure must not end this recorder.
  echo "$ended" > "$base.ended" || true
  cpu=$(children_cpu "$base.times" 2> /dev/null || true)
  drain_stamper "$stamper" "$base"
  # A `times` that could not be written or read leaves the CPU unknown, written `?` as the wait
  # writes it for an entry with no seconds at all: a 0.0 here would be a wrong number that reads
  # as a true one.
  awk -v q="$queued" -v a="$started" -v b="$ended" -v c="$cpu" \
    'BEGIN { printf "%.1f %s %.1f\n", b - a, (c == "" ? "?" : sprintf("%.1f", c)), a - q }' > "$base.seconds"
  # Last of all, and by a rename: an entry with a status has a line record that is whole or says
  # it is not.
  echo "$status" > "$base.exit.tmp"
  mv "$base.exit.tmp" "$base.exit"
}

# stamper_reading <base> <pid>: true once the stamper has opened the entry's log and its stamp
# file, which is when that file appears; false when it is gone without one, or when two seconds
# pass without it from the moment the harness's slot is granted, which is when run calls this.
# An interpreter that has not started by then is one no harness's start may wait on, and a
# stamper that began after the harness would give every line the harness had already written the
# time of its own first read.
stamper_reading() {
  local base=$1 pid=$2 tries=0
  until [[ -e $base.lines ]]; do
    if ((tries >= 40)) || ! kill -0 "$pid" 2> /dev/null; then
      return 1
    fi
    sleep 0.05
    tries=$((tries + 1))
  done
}

# drain_stamper <pid or empty> <base>: asks the stamper to read the harness's log to its end,
# waits for it, and writes the log's size beside what it stamped, so a reader can see whether the
# record covers the whole log. The harness has ended, its seconds are already measured and its
# entry has written `.ended`, from which no count gives it a slot, so this costs the wait step
# alone: a queued harness starts while this runs. Every write here is guarded: a record that
# cannot be written leaves the entry incomplete, never a harness that passed without its status,
# which the wait would judge a failure. A stamper still there after 30 s is killed and the entry
# left without its `.lines.end`, since a wait that hung here would end with the job's timeout and
# judge nothing.
drain_stamper() {
  local pid=$1 base=$2 tries=0
  [[ -n $pid ]] || return 0
  touch "$base.lines.stop" 2> /dev/null || true
  while ((tries < 600)) && kill -0 "$pid" 2> /dev/null; do
    sleep 0.05
    tries=$((tries + 1))
  done
  kill -KILL "$pid" 2> /dev/null || true
  wait "$pid" 2> /dev/null || true
  if stat -c %s "$base.log" > "$base.logbytes.tmp" 2> /dev/null; then
    mv "$base.logbytes.tmp" "$base.logbytes" 2> /dev/null || true
  else
    rm -f "$base.logbytes.tmp" || true
  fi
}

# children_cpu <file of `times` output>: the user plus system seconds of its second line, the
# shell's children.
children_cpu() {
  awk 'NR == 2 { split($1, u, /[ms]/); split($2, s, /[ms]/); printf "%.3f", u[1] * 60 + u[2] + s[1] * 60 + s[2] }' "$1"
}

wait_all() {
  local allow_none="" stopped=""
  while (($# > 0)); do
    case $1 in
      --allow-none) allow_none=1 ;;
      # The job was cancelled: judge the harnesses as they are now, without waiting for any.
      --now) stopped=1 ;;
      *)
        echo "::error::harness-pool: wait takes only --allow-none and --now, not '$1'" >&2
        exit 2
        ;;
    esac
    shift
  done
  local entries=("$dir"/*.name)
  if [[ ! -e ${entries[0]} ]]; then
    # How many harnesses this wait judged, written before it judges any, for a reader of the
    # record the job uploads: a job that queued none is a job with no case to find, where a job
    # whose record is missing is one whose cases were lost, and nothing else tells the two apart.
    # Guarded, here and below: no part of the record may change what this wait says.
    { mkdir -p "$dir" && echo 0 > "$dir/judged"; } 2> /dev/null || true
    if [[ -n $allow_none ]]; then
      echo "harness-pool: no harness was queued under $dir, and --allow-none: nothing to judge"
      exit 0
    fi
    echo "::error::harness-pool: no harness was queued under $dir, so there is nothing to judge" >&2
    exit 2
  fi
  echo "${#entries[@]}" > "$dir/judged" 2> /dev/null || true
  trap 'stopped=1' INT TERM
  # Said once the trap is set, so a reader of the log, and the test, know the wait now answers a
  # TERM with its report.
  if [[ -n $stopped ]]; then
    echo "harness-pool: the job was cancelled; judging the ${#entries[@]} harnesses queued as they are now"
  else
    echo "harness-pool: waiting for the harnesses still running of ${#entries[@]} queued"
  fi
  # A draining entry holds no slot but has no status yet, so the wait waits for it too.
  while [[ -z $stopped ]] && count && ((running + draining + waiting > 0)); do
    # A TERM to the wait's group ends this sleep too, with 143; errexit must not take that for
    # the wait's own failure before the trap's report.
    sleep "$poll" || true
  done
  # The report itself ignores a further INT or TERM (the runner follows a cancel's INT with a
  # TERM 7.5 s later), and so does every process it starts, which inherits that: a signal
  # landing in a subshell of the report would otherwise end it half written. The runner's KILL
  # still ends it.
  trap '' INT TERM
  local entry base name status wall cpu waited now ended_at result bound_minutes bound_seconds log_now log_was failed=0 unfinished=0
  now=$(date +%s.%N)
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
    printf '| Harness | Wall seconds | CPU seconds | Seconds waiting for a slot | Result |\n| --- | --- | --- | --- | --- |\n' >> "$GITHUB_STEP_SUMMARY"
  fi
  for entry in "${entries[@]}"; do
    base=${entry%.name}
    name=$(< "$entry")
    if [[ -n $stopped ]] && alive "$base"; then
      unfinished=$((unfinished + 1))
      if [[ ! -e $base.started ]]; then
        echo "::error title=$name::was still waiting for a slot when the wait was stopped, and never ran"
        if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
          printf '| %s | - | - | ? | never started |\n' "$name" >> "$GITHUB_STEP_SUMMARY"
        fi
        continue
      fi
      if [[ -e $base.ended ]]; then
        # Its harness has ended and its recorder was draining the stamper (drain_stamper) when
        # the wait was stopped: the harness ran for a known time, and its status is not written.
        read -r ended_at 2> /dev/null < "$base.ended" || ended_at=""
        wall=$(awk -v a="$(< "$base.started")" -v b="$ended_at" 'BEGIN { if (b == "") print "?"; else printf "%.1f", b - a }')
        echo "::group::$name: ended after $wall s, and its status was not yet written when the wait was stopped; its log"
        cat "$base.log" 2> /dev/null || echo "harness-pool: $name left no log"
        echo "::endgroup::"
        echo "::error title=$name::ended after $wall s, but its recorder had not yet written its status when the wait was stopped; its log is in the group of that name above"
        if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
          printf '| %s | %s | ? | ? | unfinished |\n' "$name" "$wall" >> "$GITHUB_STEP_SUMMARY"
        fi
        continue
      fi
      wall=$(awk -v a="$(< "$base.started")" -v b="$now" 'BEGIN { printf "%.1f", b - a }')
      echo "::group::$name: still running after $wall s when the wait was stopped; its log so far"
      cat "$base.log" 2> /dev/null || echo "harness-pool: $name has no log yet"
      echo "::endgroup::"
      echo "::error title=$name::was still running after $wall s when the wait was stopped; its log so far is in the group of that name above"
      if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
        printf '| %s | %s | ? | ? | unfinished |\n' "$name" "$wall" >> "$GITHUB_STEP_SUMMARY"
      fi
      continue
    fi
    status=$(cat "$base.exit" 2> /dev/null || echo 'none: its recorder ended without writing one')
    # A recorder killed before its harness ended left the harness running in its own session:
    # end it, if no count has yet (a --now report counts nothing).
    lost_running "$base" || true
    # A process the harness left behind can write to its log after the stamper read it to the
    # end: what the recorder wrote as the log's size is then not the log's size, and a reader
    # comparing the two numbers would take a part of the log for the whole of it. Drop that size
    # where the log has grown since, so the entry's record says it is a part. Guarded: this is
    # the record, and the record decides nothing here.
    if [[ -e $base.logbytes ]]; then
      log_now=$(stat -c %s "$base.log" 2> /dev/null || true)
      log_was=$(cat "$base.logbytes" 2> /dev/null || true)
      if [[ $log_now != "$log_was" ]]; then
        rm -f "$base.logbytes" || true
      fi
    fi
    read -r wall cpu waited 2> /dev/null < "$base.seconds" || { wall='?'; cpu='?'; waited='?'; }
    echo "::group::$name: exit $status, $wall s, $cpu s of CPU, after $waited s waiting for a slot"
    cat "$base.log" 2> /dev/null || echo "harness-pool: $name left no log"
    if [[ -s $base.recorder.log ]]; then
      echo "harness-pool: what the recorder of $name printed:"
      cat "$base.recorder.log"
    fi
    echo "::endgroup::"
    result=pass
    if [[ $status != 0 ]]; then
      failed=$((failed + 1))
      result="fail ($status)"
      # timeout exits 124 at the bound, or 137 when the harness ignored that and took the kill.
      bound_minutes="" bound_seconds=""
      if [[ -e $base.bound ]]; then
        read -r bound_minutes bound_seconds < "$base.bound"
      fi
      if [[ ($status == 124 || $status == 137) && -n $bound_seconds ]] &&
        awk -v w="$wall" -v s="$bound_seconds" 'BEGIN { exit !(w >= s) }'; then
        echo "::error title=$name::ran past its bound of $bound_minutes minutes and was stopped (exit $status); its log is in the group of that name above"
      else
        echo "::error title=$name::exited $status after $wall s; its log is in the group of that name above"
      fi
    fi
    if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
      printf '| %s | %s | %s | %s | %s |\n' "$name" "$wall" "$cpu" "$waited" "$result" >> "$GITHUB_STEP_SUMMARY"
    fi
  done
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
    for entry in "${entries[@]}"; do
      base=${entry%.name}
      if [[ -s $base.step-summary ]]; then
        printf '\n### %s\n\n' "$(< "$entry")" >> "$GITHUB_STEP_SUMMARY"
        cat "$base.step-summary" >> "$GITHUB_STEP_SUMMARY"
      fi
    done
  fi
  if [[ -n $stopped ]]; then
    echo "harness-pool: ${#entries[@]} harnesses, $failed failed, $unfinished unfinished when the wait was stopped"
    exit 1
  fi
  echo "harness-pool: ${#entries[@]} harnesses, $failed failed"
  ((failed == 0))
}

case ${1:-} in
  start)
    shift
    start "$@"
    ;;
  wait)
    shift
    wait_all "$@"
    ;;
  run)
    shift
    run "$@"
    ;;
  *)
    echo "::error::harness-pool: usage: harness-pool.sh start [--minutes <n>] <name> <command> [<argument>...] | harness-pool.sh wait [--allow-none] [--now]" >&2
    exit 2
    ;;
esac
