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
# The two limits, told apart by the answer alone, its status and its headers, as GitHub's
# documentation defines each (REST API, "Rate limits for the REST API", and GraphQL API, "Rate
# limits and query limits for the GraphQL API", each under "Exceeding the rate limit"); never by
# its message, which is the server's text: a gateway's 504 can say "API rate limit exceeded" as
# well as GitHub's own refusal does.
#   * The hourly budget: a 403 or a 429, or the 200 GraphQL answers its errors with, carrying
#     x-ratelimit-remaining: 0. The helper reads the reset off GET /rate_limit, which GitHub
#     serves free of the budget it reports ("Accessing this endpoint does not count against your
#     REST API rate limit", docs.github.com, REST API, "Rate limit"). The core and graphql budgets
#     are read, the two the callers spend, and the latest reset of those at 0 is waited for;
#     neither at 0 means the window turned over since the refusal, and the command runs once more
#     at once.
#   * A secondary rate limit, which spends no budget: a 403, a 429 or that 200 carrying a
#     retry-after header, whose seconds are waited. GitHub asks for at least a minute where a
#     secondary limit's answer names no retry-after, but such an answer differs from any other 403
#     by its message alone, so the helper does not wait for one: it is the command's own failure,
#     which gh_read's ladder, or the caller, answers.
# Any other answer, a 504 whatever its message says, is the command's own failure.
#
# Where the answer comes from. While the command runs, the first gh on PATH is the helper's own, a
# script in its work directory that runs the real gh (_gh_read_gh), with that directory taken off
# PATH again first: a gh that checks it is the first gh on PATH, as a Legion pane's `legion gh`
# does, still finds itself. So a command that runs gh by name through any wrapper (bash -c, or a
# shell function that runs it under timeout) hands the helper the answer of the last gh it ran. gh
# prints no header of an answer but under GH_DEBUG=api, which logs each request and its answer,
# body and all, from a `* Request at` line to a `* Request took` line, so the helper's gh runs
# `gh api` under GH_DEBUG=api, keeps the answer's status and headers, and hands on gh's stderr
# without the log. Only a `gh api` without --paginate: one request, whose log ends at the last
# `took` line, which a line of the body cannot move. gh release view, edit and upload send a
# GraphQL lookup and a REST request together, and a paginated read sends one per page, so a log of
# theirs holds more than one answer's body (a draft release's name and body, ahead of
# release-push.sh's leak guard, or a raw page) and no one answer's headers that could be picked out
# of it; a redirect gh follows puts the next request's log after the redirect's body, likewise.
# Those, gh's other commands, and a gh api whose log holds no answer (a failed connection, or a
# stand-in gh that logs nothing) leave an answer unread. When such a command fails with 1, gh's
# status for an answer it refuses, the helper asks GET /rate_limit about the budget that command
# spends: graphql for `gh api graphql`, core for any other `gh api` route, and either for gh's
# other commands, which may send both. That budget at 0 is the hourly budget's refusal; their
# secondary limit is the command's own failure. One false wait remains: such a command that fails
# for its own reason (a 404, say) while its budget is at 0, as when its own call spent the last
# unit, or, for gh's other commands, while the other budget is. A command that ran no gh by name
# (git, say) spends no budget, and its failure is its own.
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
# This file's absolute path, for the gh the helper puts first on PATH, which sources it: a caller
# may change directory after sourcing it by a relative path.
_GH_READ_SOURCE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")

# Every status is taken with `|| status=$?` and every test sits in an `if`, so a caller under
# `set -e` (and inherit_errexit) reaches the decision below rather than leaving at the first
# refused run.
gh_wait_reset() {
  local into='' dir status=0 kind reset=0 seconds until wake now pause limit deadline why failure=''
  if [ "${1:-}" = --stderr ]; then
    into=$2
    shift 2
  fi
  dir=$(mktemp -d) || return 1
  _gh_read_shim "$dir"
  _gh_read_run "$dir" "$@" || status=$?
  kind=$(_gh_read_refusal "$status" "$dir")
  if [ -n "$kind" ]; then
    case $kind in
      budget)
        if ! reset=$(_gh_read_reset 2> "$dir/probe"); then
          failure="GitHub refused this call for its API rate limit, and its rate_limit route did not say when the budget resets ($(head -n 1 "$dir/probe"))"
          reset=0
        fi
        ;;
      # Read off the rate_limit route already, for a command whose answer the helper cannot read.
      budget\ *) reset=${kind#budget } kind=budget ;;
      secondary\ *) seconds=${kind#secondary } kind=secondary ;;
    esac
    # Read after the rate_limit read, which can take its whole 30 s bound: a clock from before it
    # would sleep that long past the reset, into the margin the deadline check left.
    now=$(date +%s)
    wake=$now
    if [ "$kind" = budget ] && [ "$reset" -gt 0 ]; then
      wake=$((reset + 1 > now ? reset + 1 : now))
      until="the API budget is spent until $(_gh_read_utc "$reset")"
    elif [ "$kind" = secondary ]; then
      # A second past it, as for a reset: `now` is whole seconds, up to one behind the clock, so
      # now + seconds can fall up to a second short of the wait GitHub asked for.
      wake=$((now + seconds + 1))
      until="GitHub's secondary rate limit asks this call to wait $seconds s (its retry-after), until $(_gh_read_utc "$wake")"
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
      if [ -n "$(_gh_read_refusal "$status" "$dir")" ]; then
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

# _gh_read_shim <dir>: writes <dir>/bin/gh, the gh the commands of one gh_wait_reset meet first
# on PATH, and <dir>/gh, the path of the real one (the gh on PATH now, or the real gh an
# enclosing gh_wait_reset named in GH_READ_GH, so a helper run inside another's command does not
# take that one's gh for the real gh). Writes neither when there is no gh on PATH: the command
# then runs as it is and leaves no answer to read.
_gh_read_shim() {
  local real=${GH_READ_GH:-}
  [ -n "$real" ] || real=$(type -P gh) || return 0
  mkdir -p "$1/bin" || return 0
  printf '%s\n' "$real" > "$1/gh"
  printf '#!/usr/bin/env bash\nsource %q && _gh_read_gh %q "$@"\n' "$_GH_READ_SOURCE" "$1/answer" > "$1/bin/gh"
  chmod +x "$1/bin/gh"
}

# _gh_read_run <dir> <command>...: one run, its stdout kept in <dir>/out and its stderr in
# <dir>/err, with <dir>/bin first on PATH, so the last gh the command ran by name writes
# <dir>/answer (_gh_read_gh). The answer is emptied first, so a run whose command ran no gh that
# way leaves it empty.
_gh_read_run() {
  local dir=$1 status=0
  shift
  : > "$dir/answer"
  if [ -x "$dir/bin/gh" ]; then
    GH_READ_GH=$(<"$dir/gh") PATH="$dir/bin:$PATH" "$@" > "$dir/out" 2> "$dir/err" || status=$?
  else
    "$@" > "$dir/out" 2> "$dir/err" || status=$?
  fi
  return "$status"
}

# _gh_read_gh <answer file> <gh argument>...: the real gh ($GH_READ_GH) with those arguments, as
# the gh <dir>/bin/gh runs, with <dir>/bin taken off the front of PATH (the header says why). The
# answer file gets an answer (_gh_read_answer), or `unread <budget>...`, the budgets the command
# can spend (_gh_read_budgets), where no answer can be read. `gh api` without --paginate, one
# request, runs under GH_DEBUG=api; every other gh command, a paginated `gh api` among them
# (several requests, each page's body inside the log), is unread and runs as it is, by exec.
#
# gh runs as a child here, so a TERM, INT or HUP sent to this process alone (`timeout
# --foreground`) is passed on to it, and the log, in the helper's work directory, is removed once
# gh ends. The traps are set before gh starts, and a signal that comes before its pid is known is
# passed on once it is. gh starts from a subshell in the background: a background simple command
# would start with INT and QUIT ignored and stdin at /dev/null, and gh keeps an ignored INT until
# it sets a handler, while a subshell's exec starts it with the signals this process started with
# (measured with bash 5.2.21; gh-read.test.sh holds it). The subshell hands gh this process's
# stdin (`gh api --input -` reads it), or none where the caller closed it.
_gh_read_gh() {
  local answer=$1 status=0 arg traced='' trace pid='' pending='' stdin='' budgets
  shift
  PATH=${PATH#"${answer%/answer}/bin:"}
  budgets=$(_gh_read_budgets "$@")
  if [ "${1:-}" = api ]; then
    traced=yes
    for arg in "$@"; do
      [ "$arg" != --paginate ] || traced=''
    done
  fi
  if [ -z "$traced" ] || ! trace=$(mktemp "$answer.trace.XXXXXX"); then
    echo "unread $budgets" > "$answer"
    exec "$GH_READ_GH" "$@"
  fi
  { exec {stdin}<&0; } 2> /dev/null || stdin=''
  trap 'if [ -n "$pid" ]; then kill -s TERM "$pid" 2> /dev/null; else pending=TERM; fi' TERM
  trap 'if [ -n "$pid" ]; then kill -s INT "$pid" 2> /dev/null; else pending=INT; fi' INT
  trap 'if [ -n "$pid" ]; then kill -s HUP "$pid" 2> /dev/null; else pending=HUP; fi' HUP
  (
    export GH_DEBUG=api
    if [ -n "$stdin" ]; then
      exec "$GH_READ_GH" "$@" <&"$stdin" {stdin}<&- 2> "$trace"
    fi
    exec "$GH_READ_GH" "$@" <&- 2> "$trace"
  ) &
  pid=$!
  [ -z "$pending" ] || kill -s "$pending" "$pid" 2> /dev/null
  [ -z "$stdin" ] || exec {stdin}<&-
  # A signal trapped above ends the wait with 128 + its number while gh still runs: wait again.
  while :; do
    status=0
    wait "$pid" || status=$?
    if [ "$status" -le 128 ] || ! kill -0 "$pid" 2> /dev/null; then
      break
    fi
  done
  trap - TERM INT HUP
  _gh_read_answer "$trace" "$answer" "$budgets" >&2
  rm -f "$trace"
  return "$status"
}

# _gh_read_budgets <gh argument>...: the budgets a gh command can spend, as GET /rate_limit names
# them: `graphql` for `gh api graphql`, `core` for any other `gh api` route (its first argument
# that is no flag and no flag's value), and `core graphql` for gh's other commands.
_gh_read_budgets() {
  local arg skip='' route=''
  if [ "${1:-}" != api ]; then
    echo core graphql
    return
  fi
  shift
  for arg in "$@"; do
    if [ -n "$skip" ]; then
      skip=''
      continue
    fi
    case $arg in
      -X | --method | -H | --header | -f | --raw-field | -F | --field | --input | -q | --jq | \
        -t | --template | -p | --preview | --hostname | --cache) skip=yes ;;
      -*) ;;
      *)
        route=$arg
        break
        ;;
    esac
  done
  if [ "$route" = graphql ]; then echo graphql; else echo core; fi
}

# _gh_read_answer <log> <answer file> <budgets>: prints the lines of gh's own in a GH_DEBUG=api log
# and writes its answer to the answer file. The log of one request runs from the first `* Request
# at` line to the last `* Request took` line, and is left out whole: the answer's body is printed
# inside it as it came, so a raw body's line can read like any of gh's own markers, and only the
# last `took` is surely gh's (the body precedes it). The answer file gets the status off the first
# `< HTTP/` line, the URL off the `* Request to` line before it, then the headers after the
# status, one `Name: value` a line, to the first line that is no header; the body comes after that
# line, so nothing in it is read as a header. A 3xx is a redirect gh follows, and the next
# request's log comes after the redirect's body, so no line of it is surely gh's; a log with no
# answer (a failed connection) has none to read: both write `unread <budgets>`. Lines before the
# request and after its end are gh's own and are printed as they came (gh closes a request on a
# failed connection too, then prints its error), and a request gh never closed (gh killed
# mid-request) ends the log with nothing of gh's own after it.
_gh_read_answer() {
  awk -v answer="$2" -v budgets="$3" '
    NR == FNR {
      if (!first && /^\* Request at /) first = FNR
      if (/^\* Request took /) last = FNR
      next
    }
    !first || FNR < first || (last >= first && FNR > last) { print; next }
    !code && /^\* Request to / { url = $4; next }
    !code && /^< HTTP\// { code = $3; next }
    code && !body && /^< / { headers = headers substr($0, 3) "\n"; next }
    code { body = 1 }
    END {
      if (code == "" || code ~ /^3/) print "unread " budgets > answer
      else printf "%s\n%s\n%s", code, url, headers > answer
    }
  ' "$1" "$1"
}

# _gh_read_refusal <status> <dir>: what a failed run's answer (<dir>/answer) says, by its status
# and headers alone (the header above): `secondary <seconds>` for a 403 or a 429, or a 200 from
# GraphQL (the path /graphql, or GitHub Enterprise Server's /api/graphql; a REST route such as
# repos/o/graphql only ends in it), with a retry-after; `budget` for one with
# x-ratelimit-remaining: 0; nothing for any other answer (a REST 200 that spent the last unit
# before gh failed on its own, say), or where no gh ran by name (an empty answer). An unread
# answer is decided by GitHub's rate_limit route: `budget <reset>` when it shows a budget the
# command spends at 0, nothing when it shows none or does not answer. It is asked only when the
# command exited 1, which is how gh exits on an answer it refuses: a command its caller's timeout
# ended (124) or a signal killed was answered nothing, and the read would add its 30 s bound to
# every such attempt of an outage that holds connections open.
_gh_read_refusal() {
  local code url path retry reset
  [ "$1" -ne 0 ] || return 0
  code=$(sed -n 1p "$2/answer")
  url=$(sed -n 2p "$2/answer")
  path=${url#*://}
  path=/${path#*/}
  path=${path%%\?*}
  case $code in
    unread\ *)
      # The budgets are words, unquoted on purpose.
      # shellcheck disable=SC2086
      if [ "$1" -eq 1 ] && reset=$(_gh_read_reset ${code#unread } 2> /dev/null) && [ "$reset" -gt 0 ]; then
        echo "budget $reset"
      fi
      return 0
      ;;
    403 | 429) ;;
    200) [ "$path" = /graphql ] || [ "$path" = /api/graphql ] || return 0 ;;
    *) return 0 ;;
  esac
  retry=$(_gh_read_header retry-after "$2/answer")
  if [[ $retry =~ ^[0-9]+$ ]]; then
    echo "secondary $retry"
  elif [ "$(_gh_read_header x-ratelimit-remaining "$2/answer")" = 0 ]; then
    echo budget
  fi
}

# _gh_read_header <name> <answer file>: the value of that header (any case), its trailing CR off.
_gh_read_header() {
  local value
  value=$(awk -F': *' -v name="$1" 'NR > 2 && tolower($1) == name { value = $2 } END { print value }' "$2")
  echo "${value%$'\r'}"
}

# _gh_read_reset [<budget>...]: prints the latest reset (epoch seconds) of the named budgets (core
# and graphql when none is named) that GET /rate_limit reports at 0, or 0 when none is. Fails with
# gh's words on stderr when the route does not answer, bounded like every read the ladder makes.
_gh_read_reset() {
  local reset budget picks=''
  [ "$#" -gt 0 ] || set -- core graphql
  for budget in "$@"; do
    picks+="${picks:+, }.resources.$budget"
  done
  reset=$(timeout 30 gh api rate_limit \
    --jq "[$picks | select(. != null and .remaining == 0) | .reset] | max // 0") ||
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
