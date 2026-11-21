#!/usr/bin/env bash
# The two ways the scripts beside this file, and the workflow steps that source it, make a GitHub
# call with the run's token: gh_wait_reset, which every call goes through, and gh_read, a retry
# ladder over it for the calls a single refusal must decide nothing for.
#
# Sourced, never run: `source "$(dirname "${BASH_SOURCE[0]}")/gh-read.sh"`.
#
# gh_wait_reset [--stderr <file>] <command>...: runs the command once. When it fails and its
# stderr carries GitHub's refusal for the API rate limit (gh prints the API's own message, "API
# rate limit exceeded for installation ID <n>", for a 403 or a 429), it reads when the budget
# resets and, when that falls before the job's limit (or the step's, below) by at least
# GH_WAIT_MARGIN seconds, logs one ::warning:: line naming the reset and the limit, sleeps until
# a second past the reset and runs the command once more. When the reset falls later, or no limit
# can be read, it runs nothing more and fails by name at once. Any other failure is the
# command's own: its stdout, its stderr and its exit status reach the caller as a bare call's
# would. It returns the status of the run it ended on, or 75 when it failed by name: the budget
# spent past the limit, no limit to wait within, a reset it could not read, or a budget still
# spent after the wait. Every line of its own goes to stderr; stdout holds the stdout of the run
# it ended on and nothing else, so a run it repeats leaves no half page in the caller's hands.
#
# --stderr <file> writes the stderr of the run it ended on to <file> instead of stderr, for a
# caller that reads gh's words (a 404 told apart from a refusal); an earlier run's stderr, and
# every line of the helper's own, still reach stderr, where the job log holds them.
#
# Where the reset comes from. A refusal's x-ratelimit-reset header names it, but gh prints no
# header of a refused answer (only GH_DEBUG=api does, with every body beside it), so the helper
# reads the same reset off GET /rate_limit, which GitHub serves free of the budget it reports
# ("Accessing this endpoint does not count against your REST API rate limit", docs.github.com,
# REST API, "Rate limit"). The core and graphql budgets are read, the two the callers spend; the
# latest reset of those at 0 is waited for. Neither at 0 means the window turned over since the
# refusal, and the command runs once more at once. A secondary rate limit ("You have exceeded a
# secondary rate limit") is no spent budget: its message names no exceeded limit, and it fails
# as any other refusal does (gh_read's ladder retries it as before).
#
# Where the limit comes from. GitHub enforces a job's timeout-minutes from the job's start, and
# a step's from the step's, and the runner exposes neither the limit nor the start to a step.
# So both limits are passed in, as the step's env sets them from its job's and its own
# timeout-minutes:
#   JOB_TIMEOUT_MINUTES   the job's timeout-minutes, measured from the job's start;
#   STEP_TIMEOUT_MINUTES  optional: the step's own timeout-minutes, measured from the step's.
# and both starts are read off the process tree: GitHub's runner starts one Runner.Worker process
# for each job (actions/runner src/Runner.Listener/JobDispatcher.cs, the worker it spawns per job
# request), before the job's first step, and each step's shell is that process's child. The
# nearest Runner.Worker among this shell's parents is the job's start, and its child on the way
# down is the step's. The earlier of the two limits set is the deadline. With neither set, with
# no Runner.Worker among the parents (a container job, whose steps run in another process
# namespace, or a run outside Actions), the helper has no deadline and does not wait.
#
# gh_read <command>...: runs the command through gh_wait_reset, and on any failure but a budget
# it could not wait for runs it again twice, GH_RETRY_SECONDS (10 by default; the tests set 0)
# and then twice as many seconds apart. It prints the stdout of the attempt that succeeded and
# nothing else; every attempt's stderr goes to the caller's stderr, where the job log holds it.
# It returns 0 for the attempt that succeeded, 75 when gh_wait_reset failed by name (another
# attempt within seconds cannot find a budget that resets later), and 1 when all three were
# refused otherwise, which is the caller's to answer: a refused jobs list leaves ci-ok-report.sh
# with no result to name for the jobs that have not ended, a refused post leaves no ci-ok at all,
# and a refused PATCH leaves a stale ci-ok on the head. Its callers: the jobs list of
# ci-ok-report.sh, the check-run post of post-check-run.sh, the check-run list and each PATCH of
# ci-ok-reconcile.sh, the check-run and annotation reads of runner-shutdown.sh, and the jobs read
# of dispatch-browser-run-reads.sh, which bounds each attempt with `timeout 30`. GitHub refuses
# a call now and then (a 502, a secondary rate limit), and ci-ok, the check main's ruleset
# requires, is posted from what one call answered.

# Seconds the deadline must leave after the reset: the repeated call and what the step does after
# it run in that time. A gh call takes about a second and a paginated read a few.
GH_WAIT_MARGIN=30

# Every status is taken with `|| status=$?` and every test sits in an `if`, so a caller under
# `set -e` (and inherit_errexit) reaches the decision below rather than leaving at the first
# refused run.
gh_wait_reset() {
  local into='' dir status=0 reset limit deadline why now wake failure=''
  if [ "${1:-}" = --stderr ]; then
    into=$2
    shift 2
  fi
  dir=$(mktemp -d) || return 1
  _gh_read_run "$dir" "$@" || status=$?
  if [ "$status" -ne 0 ] && _gh_read_refused "$dir/err"; then
    if ! reset=$(_gh_read_reset 2> "$dir/probe"); then
      failure="GitHub refused this call for its API rate limit, and its rate_limit route did not say when the budget resets ($(head -n 1 "$dir/probe"))"
    elif [ "$reset" -gt 0 ] && ! limit=$(_gh_read_deadline); then
      failure="the API budget is spent until $(_gh_read_utc "$reset"), and $limit, so this step does not wait for it; re-run the job after that time"
    else
      if [ "$reset" -gt 0 ]; then
        deadline=${limit%% *} why=${limit#* }
        now=$(date +%s)
        wake=$((reset + 1 > now ? reset + 1 : now))
        if [ $((wake + GH_WAIT_MARGIN)) -gt "$deadline" ]; then
          failure="the API budget is spent until $(_gh_read_utc "$reset"), past this $why's limit at $(_gh_read_utc "$deadline"); re-run the job after the reset"
        fi
      fi
      if [ -z "$failure" ]; then
        # The refused run's words stay in the log; the run below is the one the caller gets.
        cat "$dir/err" >&2
        if [ "$reset" -gt 0 ]; then
          echo "::warning::the API budget is spent until $(_gh_read_utc "$reset"); waiting $((wake - now)) s for it, within this $why's limit at $(_gh_read_utc "$deadline"), then calling once more" >&2
          sleep $((wake - now))
        fi
        # With neither budget at 0 any more, the window turned over since the refusal, and the
        # command runs again at once.
        status=0
        _gh_read_run "$dir" "$@" || status=$?
        if [ "$status" -ne 0 ] && _gh_read_refused "$dir/err" && [ "$reset" -gt 0 ]; then
          failure="the API budget is still spent after the wait for its reset at $(_gh_read_utc "$reset"); re-run the job once GitHub's rate_limit route shows it back"
        elif [ "$status" -ne 0 ] && _gh_read_refused "$dir/err"; then
          failure="GitHub refused this call for its API rate limit twice, and its rate_limit route showed neither the core nor the graphql budget at 0 between the two; re-run the job"
        fi
      fi
    fi
  fi
  # The run it ended on: its words, then the helper's own refusal or its stdout.
  if [ -n "$into" ]; then
    cat "$dir/err" > "$into"
  else
    cat "$dir/err" >&2
  fi
  if [ -n "$failure" ]; then
    echo "::error::$failure" >&2
    status=75
  else
    cat "$dir/out"
  fi
  rm -rf "$dir"
  return "$status"
}

gh_read() {
  local attempt output status
  for attempt in 1 2 3; do
    if output=$(gh_wait_reset "$@"); then
      printf '%s' "$output"
      return 0
    else
      status=$?
    fi
    [ "$status" -ne 75 ] || return 75
    [ "$attempt" -eq 3 ] || sleep $((attempt * ${GH_RETRY_SECONDS:-10}))
  done
  return 1
}

# _gh_read_run <dir> <command>...: one run, its stdout and stderr kept in <dir>/out and <dir>/err.
_gh_read_run() {
  local dir=$1
  shift
  "$@" > "$dir/out" 2> "$dir/err"
}

# _gh_read_refused <stderr file>: whether a run's stderr carries GitHub's refusal for the API
# rate limit, as gh prints the API's message for REST ("API rate limit exceeded for ...") and
# GraphQL ("API rate limit already exceeded for ...").
_gh_read_refused() {
  grep -qiE 'rate limit (already )?exceeded' "$1"
}

# _gh_read_reset: prints the latest reset (epoch seconds) of the core and graphql budgets that
# GET /rate_limit reports at 0, or 0 when neither is. Fails with gh's words on stderr when the
# route does not answer, bounded like every read the ladder makes.
_gh_read_reset() {
  local reset
  reset=$(timeout 30 gh api rate_limit \
    --jq '[.resources.core, .resources.graphql | select(. != null and .remaining == 0) | .reset] | max // 0') ||
    return 1
  if ! [[ $reset =~ ^[0-9]+$ ]]; then
    echo "it answered '$reset', not an epoch" >&2
    return 1
  fi
  echo "$reset"
}

# _gh_read_deadline: prints `<epoch> job` or `<epoch> step`, the earlier of the limits
# JOB_TIMEOUT_MINUTES and STEP_TIMEOUT_MINUTES set and which of the two it is; or prints why there
# is none and fails.
_gh_read_deadline() {
  local job=${JOB_TIMEOUT_MINUTES:-} step=${STEP_TIMEOUT_MINUTES:-} name value
  local pid=$$ child='' line comm worker='' boot ticks job_end step_end
  local -a fields
  if [ -z "$job" ] && [ -z "$step" ]; then
    echo "this step names no time limit to wait within (JOB_TIMEOUT_MINUTES is not set)"
    return 1
  fi
  for name in JOB_TIMEOUT_MINUTES STEP_TIMEOUT_MINUTES; do
    value=${!name:-}
    if [ -n "$value" ] && ! [[ $value =~ ^[0-9]+$ ]]; then
      echo "$name is '$value', not a whole number of minutes"
      return 1
    fi
  done
  # /proc/<pid>/stat: `<pid> (<comm>) <state> <ppid> ...`, the start time 20 fields after the
  # comm, in clock ticks since boot. The comm can hold spaces and parentheses, so the fields are
  # read after its last `)`.
  while [ "$pid" -gt 1 ] && read -r line 2> /dev/null < "/proc/$pid/stat"; do
    comm=${line#*\(}
    comm=${comm%)*}
    read -ra fields <<< "${line##*) }"
    if [ "$comm" = Runner.Worker ]; then
      worker=$pid
      break
    fi
    child=$pid
    pid=${fields[1]}
  done
  if [ -z "$worker" ] || [ -z "$child" ]; then
    echo "this step's start cannot be read: no Runner.Worker process is among its parents"
    return 1
  fi
  boot=$(awk '$1 == "btime" { print $2 }' /proc/stat)
  ticks=$(getconf CLK_TCK)
  read -ra fields <<< "$(_gh_read_stat_after_comm "$worker")"
  job_end=$((boot + fields[19] / ticks + ${job:-0} * 60))
  read -ra fields <<< "$(_gh_read_stat_after_comm "$child")"
  step_end=$((boot + fields[19] / ticks + ${step:-0} * 60))
  if [ -n "$job" ] && { [ -z "$step" ] || [ "$job_end" -le "$step_end" ]; }; then
    echo "$job_end job"
  else
    echo "$step_end step"
  fi
}

# _gh_read_stat_after_comm <pid>: the fields of /proc/<pid>/stat after its comm.
_gh_read_stat_after_comm() {
  local line
  read -r line < "/proc/$1/stat"
  echo "${line##*) }"
}

_gh_read_utc() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
