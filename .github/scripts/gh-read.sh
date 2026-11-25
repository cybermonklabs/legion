#!/usr/bin/env bash
# The two ways the scripts beside this file, and the workflow steps that source it, make a GitHub
# call with the run's token: gh_wait_reset, which every call goes through, and gh_read, a retry
# ladder over it for the calls a single refusal must decide nothing for.
#
# Sourced, never run: `source "$(dirname "${BASH_SOURCE[0]}")/gh-read.sh"`.
#
# gh_wait_reset [--stderr <file>] <command>...: runs the command once. When it fails on one of
# GitHub's rate limits, it works out when GitHub will answer again and, when that falls before the
# job's limit (or the step's, below) by at least GH_WAIT_MARGIN seconds, logs one ::warning:: line
# naming the wait and the limit, sleeps until then and runs the command once more; when that time
# has passed by the moment it would sleep (a slow host), it runs the command once more at once and
# logs no wait. When that time falls past the limit, or no limit can be read, it runs nothing more
# and fails by name at once. Any other failure is the command's own: its stdout, its stderr and
# its exit status reach the caller as a bare call's would. It returns the status of the run it
# ended on, or 75 when it failed by name: a wait past the limit, no limit to wait within, a reset
# it could not read, or a refusal again after the wait. Every line of its own goes to stderr;
# stdout holds the stdout of the run it ended on and nothing else, so a run it repeats leaves no
# half page in the caller's hands.
#
# --stderr <file> writes the stderr of the run it ended on to <file> instead of stderr, for a
# caller that reads gh's words (a 404 told apart from a refusal); an earlier run's stderr, and
# every line of the helper's own, still reach stderr, where the job log holds them.
#
# The two limits, told apart by the API's own message, which gh prints for a 403 or a 429:
#   * The hourly budget ("API rate limit exceeded for installation ID <n>", or "already
#     exceeded" from GraphQL). The refusal's x-ratelimit-reset header names its reset, and the
#     helper reads the same reset off GET /rate_limit, which GitHub serves free of the budget it
#     reports ("Accessing this endpoint does not count against your REST API rate limit",
#     docs.github.com, REST API, "Rate limit"), so a command it does not run as gh itself has one
#     too. The core and graphql budgets are read, the two the callers spend, and the latest reset
#     of those at 0 is waited for; neither at 0 means the window turned over since the refusal,
#     and the command runs once more at once.
#   * A secondary rate limit ("You have exceeded a secondary rate limit", or GitHub's older "abuse
#     detection mechanism"), which spends no budget. GitHub's documentation (REST API, "Rate
#     limits for the REST API", "Exceeding the rate limit"): wait the seconds a retry-after header
#     names, and where the answer names none, at least one minute. gh prints no header of an
#     answer but under GH_DEBUG=api, which logs each request and its answer, body and all, from a
#     `* Request at` line to a `* Request took` line, so the helper runs `gh api` under
#     GH_DEBUG=api, reads the answer's headers out of that log, and hands the caller gh's stderr
#     without it. Only a `gh api` without --paginate: one request, whose log ends at the last
#     `took` line, which a line of the body cannot move (_gh_read_run). gh release view, edit and
#     upload send a GraphQL lookup and a REST request together, and a paginated read sends one per
#     page, so a log of theirs holds more than one answer's body (a draft release's name and body,
#     ahead of release-push.sh's leak guard, or a raw page). Every other command, gh's own or a
#     wrapper around it, runs as it is, leaves no headers to read, and its secondary limit takes
#     the one-minute minimum.
#
# Where the limit comes from. GitHub enforces a job's timeout-minutes from the job's start, and
# a step's from the step's, and the runner exposes neither the limit nor the start to a step.
# So both limits are passed in: the job's env sets JOB_TIMEOUT_MINUTES, and a step that carries a
# timeout-minutes of its own sets STEP_TIMEOUT_MINUTES, each the number it stands for, which
# .github/scripts/check-workflow-files.sh holds equal (and it refuses a step with a limit of its
# own that reaches this file and passes none):
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
# gh_read <command>...: runs the command through gh_wait_reset, and on any failure but a rate
# limit it could not wait for within a limit runs it again twice, GH_RETRY_SECONDS (10 by
# default; the tests set 0) and then twice as many seconds apart. A rate limit met with no limit
# set (JOB_TIMEOUT_MINUTES and STEP_TIMEOUT_MINUTES unset, or no Runner.Worker to measure from)
# keeps those two later attempts, as the ladder made them before the wait existed: one may land
# after a short reset. It prints the stdout of the attempt that succeeded and nothing else; every
# attempt's stderr goes to the caller's stderr, where the job log holds it. It returns 0 for the
# attempt that succeeded, 75 when gh_wait_reset failed by name on the attempt it stopped at (past
# a limit, another attempt within seconds cannot find a limit that lifts later), and 1 when all
# three were refused otherwise, which is the caller's to answer: a refused jobs list leaves
# ci-ok-report.sh with no result to name for the jobs that have not ended, a refused post leaves
# no ci-ok at all, and a refused PATCH leaves a stale ci-ok on the head. Its callers: the jobs
# list and the queue branch's tip of ci-ok-report.sh, the `git ls-remote` ref reads of
# ci-ok-hold.sh, the check-run post of post-check-run.sh, the check-run list and each PATCH of
# ci-ok-reconcile.sh, the check-run and annotation reads of runner-shutdown.sh, and the jobs read of
# dispatch-browser-run-reads.sh, which bounds each attempt with `timeout 30`. GitHub
# refuses a call now and then (a 502), and ci-ok, the check main's ruleset requires, is posted
# from what one call answered.

# Seconds the deadline must leave after the reset: the repeated call and what the step does after
# it run in that time. A gh call takes about a second and a paginated read a few.
GH_WAIT_MARGIN=30

# Every status is taken with `|| status=$?` and every test sits in an `if`, so a caller under
# `set -e` (and inherit_errexit) reaches the decision below rather than leaving at the first
# refused run.
gh_wait_reset() {
  local into='' dir status=0 kind reset=0 seconds named until wake now pause limit deadline why failure=''
  if [ "${1:-}" = --stderr ]; then
    into=$2
    shift 2
  fi
  dir=$(mktemp -d) || return 1
  _gh_read_run "$dir" "$@" || status=$?
  kind=$(_gh_read_refusal "$status" "$dir/err")
  if [ -n "$kind" ]; then
    if [ "$kind" = budget ] && ! reset=$(_gh_read_reset 2> "$dir/probe"); then
      failure="GitHub refused this call for its API rate limit, and its rate_limit route did not say when the budget resets ($(head -n 1 "$dir/probe"))"
      reset=0
    fi
    # Read after the rate_limit read, which can take its whole 30 s bound: a clock from before it
    # would sleep that long past the reset, into the margin the deadline check left.
    now=$(date +%s)
    wake=$now
    if [ "$kind" = budget ] && [ "$reset" -gt 0 ]; then
      wake=$((reset + 1 > now ? reset + 1 : now))
      until="the API budget is spent until $(_gh_read_utc "$reset")"
    elif [ "$kind" = secondary ]; then
      seconds=$(awk -F': *' 'tolower($1) == "retry-after" { value = $2 } END { print value }' "$dir/headers")
      seconds=${seconds%$'\r'}
      if [[ $seconds =~ ^[0-9]+$ ]]; then
        named="its retry-after"
      else
        seconds=60 named="GitHub's documented minimum, as the answer named no retry-after"
      fi
      # A second past it, as for a reset: `now` is whole seconds, up to one behind the clock, so
      # now + seconds can fall up to a second short of the wait GitHub asked for.
      wake=$((now + seconds + 1))
      until="GitHub's secondary rate limit asks this call to wait $seconds s ($named), until $(_gh_read_utc "$wake")"
    fi
    if [ -z "$failure" ] && [ "$wake" -gt "$now" ]; then
      if ! limit=$(_gh_read_deadline); then
        failure="$until, and $limit, so this step does not wait for it; re-run the job after that time"
      else
        deadline=${limit%% *} why=${limit#* }
        if [ $((wake + GH_WAIT_MARGIN)) -gt "$deadline" ]; then
          failure="$until, past this $why's limit at $(_gh_read_utc "$deadline"); re-run the job after that time"
        fi
      fi
    fi
    if [ -z "$failure" ]; then
      # The refused run's words stay in the log; the run below is the one the caller gets.
      cat "$dir/err" >&2
      now=$(date +%s)
      pause=$((wake > now ? wake - now : 0))
      if [ "$pause" -gt 0 ]; then
        echo "::warning::$until; waiting $pause s for it, within this $why's limit at $(_gh_read_utc "$deadline"), then calling once more" >&2
        sleep "$pause"
      fi
      status=0
      _gh_read_run "$dir" "$@" || status=$?
      if [ -n "$(_gh_read_refusal "$status" "$dir/err")" ]; then
        if [ "$kind" = budget ] && [ "$reset" -gt 0 ]; then
          failure="the API budget is still spent after the wait for its reset at $(_gh_read_utc "$reset"); re-run the job once GitHub's rate_limit route shows it back"
        elif [ "$kind" = secondary ]; then
          failure="GitHub refused this call for its rate limit again after the wait ($until); re-run the job"
        else
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
    # Past a limit, later attempts within seconds meet the same refusal; with no limit set, the
    # ladder's later attempts are the only retry there is, and one may land after a short reset.
    if [ "$status" -eq 75 ] && _gh_read_deadline > /dev/null; then
      return 75
    fi
    [ "$attempt" -eq 3 ] || sleep $((attempt * ${GH_RETRY_SECONDS:-10}))
  done
  [ "$status" -ne 75 ] || return 75
  return 1
}

# _gh_read_run <dir> <command>...: one run, its stdout kept in <dir>/out and its stderr in
# <dir>/err. `gh api` without --paginate, one request, runs under GH_DEBUG=api. Its log of that
# request and its answer runs from the first `* Request at` line to the last `* Request took`
# line, and is left out of <dir>/err whole: the answer's body is printed inside it as it came, so
# a raw body's line can read like any of gh's own markers, and only the last `took` is surely
# gh's (the body precedes it). The answer's headers, from the first `< HTTP/` line to the first
# line after it that is no header, go to <dir>/headers, one `Name: value` a line; the body comes
# after that line, so nothing in it is read as a header. Lines before the request and after its
# end are gh's own and are kept as they came (gh closes a request on a failed connection too,
# then prints its error), and a request gh never closed (gh killed mid-request) ends the log with
# nothing of gh's own after it. Every other command, a paginated `gh api` among them (several
# requests, each page's body inside the log), runs as it is, its stderr whole in <dir>/err.
_gh_read_run() {
  local dir=$1 status=0 arg traced=''
  shift
  # Emptied here, not by the awk below, which opens its file only when gh printed a line of its
  # own: a repeated run that printed none would otherwise hand on the refused run's words.
  : > "$dir/headers"
  : > "$dir/err"
  if [ "$1" = gh ] && [ "${2:-}" = api ]; then
    traced=yes
    for arg in "$@"; do
      [ "$arg" != --paginate ] || traced=''
    done
  fi
  if [ -z "$traced" ]; then
    "$@" > "$dir/out" 2> "$dir/err" || status=$?
    return "$status"
  fi
  GH_DEBUG=api "$@" > "$dir/out" 2> "$dir/trace" || status=$?
  awk -v err="$dir/err" -v headers="$dir/headers" '
    NR == FNR {
      if (!first && /^\* Request at /) first = FNR
      if (/^\* Request took /) last = FNR
      next
    }
    !first || FNR < first || (last >= first && FNR > last) { print > err; next }
    !answer && /^< HTTP\// { answer = 1; next }
    answer == 1 && /^< / { printf "%s\n", substr($0, 3) > headers; next }
    answer == 1 { answer = 2 }
  ' "$dir/trace" "$dir/trace"
  return "$status"
}

# _gh_read_refusal <status> <stderr file>: `budget` when a failed run's stderr carries GitHub's
# refusal for the hourly API budget, as gh prints the API's message for REST ("API rate limit
# exceeded for ...") and GraphQL ("API rate limit already exceeded for ..."); `secondary` for a
# secondary rate limit ("You have exceeded a secondary rate limit", or the older "abuse detection
# mechanism"); nothing otherwise.
_gh_read_refusal() {
  [ "$1" -ne 0 ] || return 0
  if grep -qiE 'rate limit (already )?exceeded' "$2"; then
    echo budget
  elif grep -qiE 'secondary rate limit|abuse detection mechanism' "$2"; then
    echo secondary
  fi
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
