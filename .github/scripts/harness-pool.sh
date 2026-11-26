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
#     Waits until every harness queued has ended. Prints each one's log in a group of its own,
#     in the order they were queued, headed by its name, its exit status, its wall and CPU
#     seconds and the seconds it waited for a slot; prints an ::error:: line naming each that
#     failed; writes one table row per harness to GITHUB_STEP_SUMMARY when that is set. Exits 1 when any harness exited other than 0 or
#     left no exit status. Exits 2 when no harness was queued, since a wait that judged nothing
#     must not read as a pass; --allow-none makes that 0, for a job whose every harness step has
#     an `if:` of its own, where a run whose change reached none of them rightly starts none.
#     A TERM or INT stops the wait (the runner signals a step it cancels): it then prints the log
#     so far of each harness still running, names each as unfinished with its seconds, names each
#     still waiting for a slot, and exits 1, so a harness that hung is named in the job's log
#     rather than lost with the step. --now does the same at once, for a job cancelled before its
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
# once it has a slot, and, once it ends, its exit status and seconds. A harness whose process went away without
# writing its status is a failure, never a pass.
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

# alive <base>: whether the harness of that entry has not ended. It has ended once it wrote its
# exit status, or once its recorder is gone without writing one (killed), which the wait judges a
# failure; without the second test a killed recorder would hold the wait until the job's timeout.
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
# ended, or lost with a process of their session still there), `waiting` to those still queued
# for one, and `ahead` to those queued before <base> that have not started. It forks nothing but
# for a lost entry, since every queued harness and the wait ask it twice a second.
count() {
  local entry other mine=${1:-}
  mine=${mine##*/}
  running=0 waiting=0 ahead=0
  for entry in "$dir"/*.name; do
    [[ -e $entry ]] || continue
    other=${entry%.name}
    if ! alive "$other"; then
      if lost_running "$other"; then
        running=$((running + 1))
      fi
      continue
    fi
    if [[ -e $other.started ]]; then
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
# log, and writes its seconds and then its exit status, the status last and by a rename, so an
# entry with a status is complete. The command's own session keeps this recorder alive through a
# harness that signals its whole process group, so even that harness ends with a status rather
# than with none.
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
  local base=$1 minutes=$2 queued started frac start_ms ended status=0 cpu_before cpu bound
  shift 2
  queued=$(date +%s.%N)
  export GITHUB_STEP_SUMMARY="$base.step-summary" GITHUB_OUTPUT="$base.github-output" \
    GITHUB_ENV="$base.github-env" GITHUB_PATH="$base.github-path" GITHUB_STATE="$base.github-state"
  admit "$base"
  # The CPU this recorder's children used so far, its waits for a slot, so that the harness's
  # CPU below is the harness's alone.
  times > "$base.times"
  cpu_before=$(children_cpu "$base.times")
  started=$(date +%s.%N)
  echo "$started" > "$base.started"
  if [[ -n $minutes ]]; then
    bound=$((minutes * seconds_per_minute))
    # The minutes as given, for the wait's message, and the bound in seconds, for its judgement.
    echo "$minutes $bound" > "$base.bound"
    frac=${started#*.}
    start_ms=$((${started%.*} * 1000 + 10#${frac:0:3}))
    # timeout runs in the harness's session, so its signal at the bound reaches the harness and
    # every process it started; the kill 30 s later reaches one that ignored it.
    HARNESS_POOL_DEADLINE_MS=$((start_ms + (bound - 30) * 1000)) \
      env --default-signal=INT,QUIT setsid --wait bash -c "$publish_then_run" harness-pool "$base" \
      timeout --kill-after=30 "$bound" "$@" > "$base.log" 2>&1 < /dev/null || status=$?
  else
    env --default-signal=INT,QUIT setsid --wait bash -c "$publish_then_run" harness-pool "$base" \
      "$@" > "$base.log" 2>&1 < /dev/null || status=$?
  fi
  ended=$(date +%s.%N)
  # The CPU the harness and everything it started used, user plus system: the second line of
  # `times` is this shell's children. Beside the wall seconds it says how much of the harness was
  # waiting rather than computing. `times` runs here and not in a pipeline, whose subshell has
  # no children of its own.
  times > "$base.times"
  cpu=$(children_cpu "$base.times")
  awk -v q="$queued" -v a="$started" -v b="$ended" -v c="$cpu" -v c0="$cpu_before" \
    'BEGIN { printf "%.1f %.1f %.1f\n", b - a, c - c0, a - q }' > "$base.seconds"
  echo "$status" > "$base.exit.tmp"
  mv "$base.exit.tmp" "$base.exit"
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
    if [[ -n $allow_none ]]; then
      echo "harness-pool: no harness was queued under $dir, and --allow-none: nothing to judge"
      exit 0
    fi
    echo "::error::harness-pool: no harness was queued under $dir, so there is nothing to judge" >&2
    exit 2
  fi
  trap 'stopped=1' INT TERM
  # Said once the trap is set, so a reader of the log, and the test, know the wait now answers a
  # TERM with its report.
  if [[ -n $stopped ]]; then
    echo "harness-pool: the job was cancelled; judging the ${#entries[@]} harnesses queued as they are now"
  else
    echo "harness-pool: waiting for the harnesses still running of ${#entries[@]} queued"
  fi
  while [[ -z $stopped ]] && count && ((running + waiting > 0)); do
    # A TERM to the wait's group ends this sleep too, with 143; errexit must not take that for
    # the wait's own failure before the trap's report.
    sleep "$poll" || true
  done
  # The report itself ignores a further INT or TERM (the runner follows a cancel's INT with a
  # TERM 7.5 s later), and so does every process it starts, which inherits that: a signal
  # landing in a subshell of the report would otherwise end it half written. The runner's KILL
  # still ends it.
  trap '' INT TERM
  local entry base name status wall cpu waited now result bound_minutes bound_seconds failed=0 unfinished=0
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
