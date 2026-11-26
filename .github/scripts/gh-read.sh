#!/usr/bin/env bash
# The wait the scripts beside this file, and the workflow steps that source it, put in front of a
# GitHub call made with the run's token: gh_wait_reset. It is no retry ladder. The one ladder of
# this repository's CI is retry_fetch, in .github/scripts/ci-retry.sh (dispatch://INFRA-1545, R2),
# and a call that retries runs that ladder through this wait, in one form:
#
#     gh_wait_reset [--stderr <file>] retry_fetch "<what>" <bound> -- gh ...
#
# which .github/scripts/check-download-retry.sh reads as one ladder, as it reads a bare
# retry_fetch, because of the margin below. The two artifact-list reads of
# ci-job-download-retry.yaml are the exception: they drive retry_fetch under a fault, and main
# never ran them through this wait, so they stay bare.
#
# Sourced, never run: `source "$(dirname "${BASH_SOURCE[0]}")/gh-read.sh"`. It sources
# ci-log-guard.sh, the file beside it, as ci-retry.sh does, for the block below.
#
# gh_wait_reset [--stderr <file>] <command>...: runs the command once. When it fails on one of
# GitHub's rate limits, it works out when GitHub will answer again and, when that time plus the
# command's margin (below) falls before the job's limit (or the step's, below), logs one
# ::warning:: line naming the wait and the limit, sleeps until then and runs the command once
# more; when that time has passed by the moment it would sleep (a slow host), it runs the command
# once more at once and logs no wait. When it falls later, or no limit can be read, it runs
# nothing more and fails by name at once, without sleeping. Any other failure is the command's
# own: its stdout, its stderr and its exit status reach the caller as a bare call's would. It
# returns the status of the run it ended on, or 75 when it failed by name: a wait past the limit,
# no limit to wait within, a reset it could not read, or a refusal again after the wait. Every
# line of its own goes to stderr; stdout holds the stdout of the run it ended on and nothing else,
# so a run it repeats leaves no half page in the caller's hands.
#
# The margin is the time this call may still take after the wait. For `retry_fetch <what> <bound>
# -- ...` it is that ladder's whole run, five attempts of <bound> seconds and its sleeps of 10,
# 20, 40 and 80 s, 5 x <bound> + 150 s, plus GH_WAIT_MARGIN's 30 s; for any other command it is
# GH_WAIT_MARGIN alone. So the ladder this runs again never outlives the nearer of the job's and
# the step's limit: it runs it again only when the wait, the whole ladder and GH_WAIT_MARGIN end
# before that limit, with no wait (the budget back already) as with one.
# A STATED LIMIT: the margin does not cover the later calls of the same step, and each of them is
# a ladder of its own. So a wait followed by 5xx answers on those later calls can run the step to
# its `timeout-minutes`, which the runner reports as the step's timeout, never as a pass. Main put
# no time bound on those calls.
# A STATED LIMIT: in the composed form each attempt's log lines, retry_fetch's blocks and gh's
# words in them, go to this helper's file (_gh_read_run) and reach the log only when that run of
# the ladder has ended, before the wait for a refused run and when gh_wait_reset returns for the
# run it ends on. So a step killed at its key in the middle of a ladder shows none of that
# ladder's lines.
#
# Where the caller sent stdout to stderr (`gh_wait_reset ... >&2`, read as
# `[ /dev/fd/1 -ef /dev/fd/2 ]`), the run's stdout is text for the log, and it is written inside
# a stop-commands block of its own (ci-log-guard.sh), as retry_fetch writes such a copy inside its
# attempt's block. retry_fetch cannot do that here: its stdout is this helper's file, and it reads
# that as a caller's capture. Anywhere else stdout is the caller's value, and is copied plain.
#
# A run it repeats leaves its stderr in the log inside a stop-commands block of its own, written
# before the wait: that text is the host's, and a ladder's `::error::` of exhaustion in it must not
# annotate a step whose call the run after the wait still answers. The stderr of the run it ends
# on is printed bare, as a bare call's would be, so a refusal it ends on annotates the step.
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
#     which the composed form's ladder, or the caller, answers.
# Any other answer, a 504 whatever its message says, is the command's own failure.
#
# Where the answer comes from. While the command runs, the first gh on PATH is the helper's own, a
# script in its work directory that runs the real gh (_gh_read_gh), with that directory taken off
# PATH again first: a gh that checks it is the first gh on PATH, as a Legion pane's `legion gh`
# does, still finds itself. So a command that runs gh by name through any wrapper (bash -c, or a
# shell function that runs it under timeout) hands the helper the answer of the last gh it ran.
# retry_fetch is such a function: in the composed form the answer is the one its last attempt got,
# the attempt whose status retry_fetch returns. gh prints no header of an answer but under
# GH_DEBUG=api, which logs each request and its answer, body and all, from a `* Request at` line
# to a `* Request took` line, so the helper's gh runs `gh api` under GH_DEBUG=api, keeps the
# answer's status and headers, and hands on gh's stderr without the log. Only a `gh api` without
# --paginate: one request, whose log ends at the last
# `took` line, which a line of the body cannot move. gh release view, edit and upload send a
# GraphQL lookup and a REST request together, and a paginated read sends one per page, so a log of
# theirs holds more than one answer's body (a draft release's name and body, ahead of
# release-push.sh's leak guard, or a raw page) and no one answer's headers that could be picked out
# of it; a redirect gh follows puts the next request's log after the redirect's body, likewise.
# Those, gh's other commands, and a gh api whose log holds no answer (a failed connection, or a
# stand-in gh that logs nothing) leave an answer unread. When such a command fails with 1, gh's
# status for an answer it refuses, the helper asks GET /rate_limit about the budget that command
# spends: graphql for `gh api graphql`; core for any other `gh api` route, and for `gh run`, `gh
# workflow` and `gh cache`, which call REST routes alone; and either for gh's other commands (`gh
# release`, `gh pr`), which may send both, and for a `gh workflow run` that names no --ref, whose
# default-branch lookup is GraphQL (gh 2.98.0, pkg/cmd/workflow/run/run.go). That budget at 0 is
# the hourly budget's refusal; their secondary limit is the command's own failure. One false wait
# remains: such a command that fails for its own reason (a 404, say) while its budget is at 0, as
# when its own call spent the last unit, or, for those that may send both, while the other budget
# is. A command that ran no gh by name (git, say) spends no budget, and its failure is its own.
# A STATED LIMIT: in the composed form that rate_limit read comes once, after the whole ladder,
# and its 30 s bound is not in the ladder's arithmetic, 5 x <bound> + 150 s, which
# check-download-retry.sh charges the call. So a step whose `timeout-minutes` leaves less than
# 30 s over its ladders can reach that limit in the read, after a ladder whose last attempt exited
# 1, before the helper names the failure. The runner reports that as the step's timeout, never as
# a pass. Only the key of dispatch-browser.yaml's "Reading one, the rows" holds those 30 s, by
# hand.
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
# Its callers: every GitHub call of cancel-started-act.sh, check-pr-text.sh, find-pr-payload.sh,
# ci-ok-report.sh, ci-ok-reconcile.sh, post-check-run.sh and runner-shutdown.sh, ci-ok-hold.sh's
# run read and `git ls-remote` ref reads, dispatch-browser-run-reads.sh's jobs read, and the
# steps of ci-caches.yaml, dequeued.yaml and main-bypass-check.yaml, each in the form above; and,
# with no ladder, the release calls of release-push.sh, which R2's exception (d) leaves unretried
# in the credentialed publish jobs, and check-daemon-api-contract.sh's reads.

if ! command -v ci_log_block_open > /dev/null 2>&1; then
  if [ -r "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ci-log-guard.sh" ]; then
    # shellcheck source=.github/scripts/ci-log-guard.sh
    . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ci-log-guard.sh"
  else
    echo "::error::gh-read.sh: ci_log_block_open is not defined and no ci-log-guard.sh can be read beside this file" >&2
    return 2
  fi
fi

# GH_WAIT_MARGIN, the 30 s of the margin above: for a call with a ladder it follows the ladder's
# 5 x <bound> + 150 s, so the deadline must leave that whole ladder and these 30 s after the wait;
# for a call with no ladder it is the whole margin, the one run included (the deadline check is
# `wake + margin`), where a gh call takes about a second and a paginated read a few. It covers no
# later call of the same step, each a ladder of its own (the header's stated limit on those calls).
GH_WAIT_MARGIN=30
# This file's absolute path, for the gh the helper puts first on PATH, which sources it: a caller
# may change directory after sourcing it by a relative path.
_GH_READ_SOURCE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")

# Every status is taken with `|| status=$?` and every test sits in an `if`, so a caller under
# `set -e` (and inherit_errexit) reaches the decision below rather than leaving at the first
# refused run.
gh_wait_reset() {
  local into='' dir status=0 kind reset=0 seconds until='' wake now pause limit deadline why
  local failure='' margin=$GH_WAIT_MARGIN ladder='' again token
  if [ "${1:-}" = --stderr ]; then
    into=$2
    shift 2
  fi
  # The ladder's own arithmetic, read off the call as check-download-retry.sh reads it: the bound
  # is the word after <what>, and `--` the word after that. A bound retry_fetch refuses runs no
  # attempt, so it meets no rate limit and needs no margin.
  if [ "${1:-}" = retry_fetch ] && [ "${4:-}" = -- ] && [[ ${3:-} =~ ^[0-9]+$ ]]; then
    ladder=$((5 * 10#$3 + 150))
    margin=$((ladder + GH_WAIT_MARGIN))
    again="one more run of its ladder, 5 x $((10#$3)) + 150 = $ladder s, and $GH_WAIT_MARGIN s after it"
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
    # A ladder is held to the limit even where there is nothing to wait for: run again at once, it
    # still takes up to its whole margin.
    if [ -z "$failure" ] && { [ "$wake" -gt "$now" ] || [ -n "$ladder" ]; }; then
      [ -n "$until" ] || until="GitHub refused this call for its API rate limit, and its rate_limit route shows the budget back"
      if ! limit=$(_gh_read_deadline); then
        failure="$until, and $limit, so this step does not wait for it; re-run the job after that time"
      else
        deadline=${limit%% *} why=${limit#* }
        if [ $((wake + margin)) -gt "$deadline" ]; then
          if [ -n "$ladder" ]; then
            failure="$until, and that time with $again ends at $(_gh_read_utc $((wake + margin))), past this $why's limit at $(_gh_read_utc "$deadline"); re-run the job after that time"
          else
            failure="$until, past this $why's limit at $(_gh_read_utc "$deadline"); re-run the job after that time"
          fi
        fi
      fi
    fi
    if [ -z "$failure" ]; then
      # The refused run's words stay in the log, inside a stop-commands block of their own: they
      # are the host's text, and where the command is a ladder they hold its `::error::` of
      # exhaustion, which must not annotate a step whose call the run below may still answer.
      # The run below is the one the caller gets, printed as any run this ends on is, so a
      # refusal it ends on still annotates the step.
      token=$(ci_log_token) || token=
      if [ -n "$token" ]; then
        ci_log_block_open "$token"
        cat "$dir/err" >&2 || true
        ci_log_block_close "$token"
      else
        echo "gh-read.sh: no stop-commands token could be made, so the refused run's words are not printed" >&2
      fi
      now=$(date +%s)
      pause=$((wake > now ? wake - now : 0))
      if [ "$pause" -gt 0 ]; then
        if [ -n "$ladder" ]; then
          echo "::warning::$until; waiting $pause s for it, then $again, within this $why's limit at $(_gh_read_utc "$deadline")" >&2
        else
          echo "::warning::$until; waiting $pause s for it, within this $why's limit at $(_gh_read_utc "$deadline"), then calling once more" >&2
        fi
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
  elif [ /dev/fd/1 -ef /dev/fd/2 ] && [ -s "$dir/out" ]; then
    # Text for the log, the host's as much as the command's stderr is: inside a block of its own,
    # whose markers go on stderr with it (the stream they share here). `|| token=` keeps a token
    # that cannot be made from ending a caller under errexit; the output then stays unwritten, and
    # the status says so.
    token=$(ci_log_token) || token=
    if [ -z "$token" ]; then
      echo "::error::gh-read.sh: no stop-commands token could be made, so this call's output is not printed" >&2
      status=2
    else
      ci_log_block_open "$token"
      cat "$dir/out" >&2 || status=$?
      ci_log_block_close "$token"
    fi
  else
    cat "$dir/out" || status=$?
  fi
  rm -rf "$dir"
  return "$status"
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
  # gh by name, bound to the real gh's file: check-download-retry.sh refuses a run whose command
  # word is an expansion, since it cannot name what runs there. `hash -p` sets what the name runs
  # without a change to PATH, and comes after the PATH assignment above, which empties the table.
  hash -p "$GH_READ_GH" gh
  budgets=$(_gh_read_budgets "$@")
  if [ "${1:-}" = api ]; then
    traced=yes
    for arg in "$@"; do
      [ "$arg" != --paginate ] || traced=''
    done
  fi
  if [ -z "$traced" ] || ! trace=$(mktemp "$answer.trace.XXXXXX"); then
    echo "unread $budgets" > "$answer"
    exec gh "$@"
  fi
  { exec {stdin}<&0; } 2> /dev/null || stdin=''
  trap 'if [ -n "$pid" ]; then kill -s TERM "$pid" 2> /dev/null; else pending=TERM; fi' TERM
  trap 'if [ -n "$pid" ]; then kill -s INT "$pid" 2> /dev/null; else pending=INT; fi' INT
  trap 'if [ -n "$pid" ]; then kill -s HUP "$pid" 2> /dev/null; else pending=HUP; fi' HUP
  (
    export GH_DEBUG=api
    if [ -n "$stdin" ]; then
      exec gh "$@" <&"$stdin" {stdin}<&- 2> "$trace"
    fi
    exec gh "$@" <&- 2> "$trace"
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
# that is no flag and no flag's value) and for `gh run`, `gh workflow` and `gh cache` (REST
# routes alone: the actions API), and `core graphql` for a `gh workflow run` with no --ref (its
# default-branch lookup is GraphQL) and for gh's other commands.
_gh_read_budgets() {
  local arg skip='' route=''
  case ${1:-} in
    api) ;;
    run | cache)
      echo core
      return
      ;;
    workflow)
      if [ "${2:-}" = run ]; then
        for arg in "$@"; do
          case $arg in
            -r | -r?* | --ref | --ref=*)
              echo core
              return
              ;;
          esac
        done
        echo core graphql
      else
        echo core
      fi
      return
      ;;
    *)
      echo core graphql
      return
      ;;
  esac
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
# gh's words on stderr when the route does not answer, bounded at the 30 s a `gh api` read of this
# repository's CI carries.
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
