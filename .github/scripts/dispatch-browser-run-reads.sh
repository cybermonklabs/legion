#!/usr/bin/env bash
# The two reads of its own run that the merge job of .github/workflows/dispatch-browser.yaml
# makes, in one file, so that a test can drive them under a stand-in `gh` rather than on a
# runner against GitHub.
#
# Two modes, after dispatch-browser-samples.sh's two:
#
#   .github/scripts/dispatch-browser-run-reads.sh jobs <out-file>
#   .github/scripts/dispatch-browser-run-reads.sh blobs-refused
#
# `jobs` reads this run's own attempt's jobs and writes <out-file>: the array of
# {id, name, conclusion, leg} that the merge job's two readings consume - reading one for the
# legs that owed a blob report, reading three for the legs that did not succeed.
#
# `blobs-refused` writes no file and makes no request. It prints the one `::error::` line for a
# blob download `actions/download-artifact` could not complete and exits 0: that step has
# already failed the job, and this one exists only to say what kind of failure it was.
#
# That line names the download step and this run's attempt and nothing else, because this job
# knows nothing else. The download step passes no `github-token`, so the action never reads
# `repos/<owner>/<repo>/actions/runs/<id>/artifacts`: it lists over the internal Twirp
# `ListArtifacts`, asks `GetSignedArtifactURL` for a signed URL and GETs that URL. Job
# 128735037875 of #2983, read 2026-11-08, logs `Redirecting to blob download url:
# https://<storage account>.blob.core.windows.net/actions-results/...` and then `Artifact
# download failed after 5 retries.`, so the list answered and blob storage did not; the same
# step fails with no retry at all when a digest does not match (`Digest validation failed`).
# Which of those it was is in that step's own error line in this job's log, which this mode
# cannot read, so the line names neither a route nor a retry count nor what a re-run would
# find on the run.
#
# Why the route is the attempt endpoint and not `filter=all`. The merge job read
# `.../runs/<id>/jobs?filter=all&per_page=100`, which lists every attempt's jobs, so each re-run
# makes the read heavier: measured 2026-11-08 on run 42468236584, `filter=all` answers 276 jobs
# over three pages and GitHub answers HTTP 502 on pages 2 and 3, while
# `.../attempts/<n>/jobs?per_page=100` answers 46 jobs on one page for each of attempts 1 to 6.
# A job of this run reads its own attempt, which is the one it is reporting on, so no read in
# this file uses `filter=all`.
#
# Why no grouping is left. Over `filter=all` the filter ended
# `group_by(.name) | map(max_by(.run_attempt) | …)`, which took the newest attempt of each name
# because the list held several of them. One attempt's snapshot holds one job per name, so there
# is nothing left to group and the pages map straight to the four fields.
#
# Why `--paginate --slurp` and a second `jq` remain. `gh api --paginate --jq` runs the filter
# once per page, and gh refuses `--slurp` beside `--jq` (`the --slurp option is not supported
# with --jq or --template`, gh 2.98.0), so the pages are slurped into one array and the filter
# runs as its own `jq` over that array - which is why the filter opens `.[].jobs[]`. One page is
# what this endpoint answers today, not a promise: a bigger matrix pages again, and `--paginate`
# is what keeps that a longer array rather than a short list nothing reports.
#
# The read goes through gh-read.sh's ladder, which the inline read had none of, and each attempt
# is bounded by `timeout 30`, which the ladder does not bound: a host that accepts the
# connection and then sends nothing would otherwise hang the step for as long as the job has.
# A read refused after the ladder names the endpoint, which attempt decided the failure - the
# last one the ladder made - and what that attempt did, what each earlier attempt did where it
# did something else, how many attempts that was and the run attempt, and says in those words
# that it is a failed read and not a failed row - the merge job's readings name legs, and a
# reader who meets one of these lines must not take it for one. What an attempt did is one of
# four, read from that attempt's own exit status and its own stderr and from no other attempt's:
# it stopped at the 30 s bound, where `timeout` ended it and it exited 124; it answered the HTTP
# status gh printed; it exited some other status, printing no status at all; or it left no exit
# status on record, which is an attempt whose status file was never written.
#
# Everything it reads comes from the process environment - GH_TOKEN, GITHUB_REPOSITORY,
# GITHUB_RUN_ID and GITHUB_RUN_ATTEMPT, as Actions sets them and as the merge job's `run:` body
# exports the token - and never from a step `env:` key, for the reason the shard job's head
# gives.
#
# Run from anywhere: .github/scripts/dispatch-browser-run-reads.sh jobs <out-file>, or
# .github/scripts/dispatch-browser-run-reads.sh blobs-refused
# CI runs it in dispatch-browser.yaml's merge job, at the step "Reading one, the rows" for the
# `jobs` mode and at the step "Report a refused blob download as a refused read" for
# `blobs-refused`, and its tests in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/gh-read.sh
source "$script_dir/gh-read.sh"

usage() {
  echo "::error::dispatch-browser-run-reads.sh takes a mode and its arguments:" \
    ".github/scripts/dispatch-browser-run-reads.sh jobs <out-file>, which writes this run's" \
    "attempt's jobs, or .github/scripts/dispatch-browser-run-reads.sh blobs-refused, which" \
    "prints the line for a blob download that could not be completed. It was given: $*"
  exit 1
}

# need <variable>...: refuses, naming the one that is empty or unset. A read made without one of
# these is a 404 or a 401 whose message names neither the run nor the variable that was missing.
need() {
  local input
  for input in "$@"; do
    if [ -z "${!input:-}" ]; then
      echo "::error::dispatch-browser-run-reads.sh needs $input in its environment"
      exit 1
    fi
  done
}

# describe_attempt <attempt directory> <attempt>: what one attempt of the ladder did, in the
# words the refusal line uses, read from that attempt's own exit status and its own stderr and
# from no other attempt's, in four branches and holding no ` and ` of its own: the line joins
# one attempt's clause to the next with ` and `, so a clause holding one of its own reads as
# two attempts. Exit status 124 is `timeout` having ended that attempt at the 30 s bound.
# Otherwise the last `HTTP <n>` gh printed on that attempt's stderr is what that attempt was
# answered. Otherwise the exit status that was recorded is named as it was recorded, rather
# than the attempt being called refused: gh exits 1 where it was refused with no status of its
# own - a resolution failure, a connection refused - but `timeout` exits 127 where it could not
# run gh at all (`timeout: failed to run command 'gh'`) and the attempt exits 137 for a gh that
# was killed, and nothing refused anything in either of those. Otherwise `status.<n>` holds
# nothing, and the words name that file and nothing further: an attempt that reaches this
# branch may well have exited. Measured 2026-11-08, with the wrapper of the third of three
# attempts killed, that wrapper exited 137 and 137 is the status the ladder's attempt saw,
# while nothing had written `status.3`.
#
# What keeps a missing file from ending this read is the `|| true` on each of the two reads,
# not the `2>/dev/null` beside it, which only keeps `cat: <dir>/status.3: No such file or
# directory` out of the job log. Measured 2026-11-08 against the test file beside this one:
# both `|| true` taken off reads 118 passed, 17 failed, and both `2>/dev/null` taken off reads
# 135 passed, 0 failed, where a by-hand drive of a killed wrapper against that second copy
# prints that one line beside the refusal line and the suite prints neither.
#
# Two shapes reach the fourth branch. One is a wrapper killed during an attempt: the
# redirection has made `stderr.<n>` and nothing has written `status.<n>`, and where gh had
# printed a status on that stderr before the wrapper died, the attempt is described from it
# rather than by these words. The test file's case "a killed wrapper whose gh had printed a
# status before it died" is what holds that order: a copy asking whether the status file is
# missing before it greps the stderr reads 133 passed, 2 failed, measured 2026-11-08. The
# other shape is a wrapper that never ran at all - driven 2026-11-08 with gh_read given a
# command that does not exist, the line read `attempt 0, which decided the failure, left no
# exit status on record, printing no HTTP status; the ladder made 0 attempts`, `attempts`
# still holding the 0 it was initialised to.
#
# A wrapper killed before it counts its attempt is described by no branch of its own, and it
# misnumbers the attempts that follow it as well as shortening the count: driven 2026-11-08
# with the second of three wrappers killed before its count, the attempt that decided the
# failure - the ladder's third - was described from its own files under the number 2, in a
# line reading `the ladder made 2 attempts`.
describe_attempt() {
  local dir=$1 attempt=$2 attempt_status http
  attempt_status=$(cat "$dir/status.$attempt" 2>/dev/null || true)
  if [ "$attempt_status" = 124 ]; then
    echo "stopped at its 30 s bound"
    return
  fi
  http=$(grep -o 'HTTP [0-9][0-9][0-9]' "$dir/stderr.$attempt" 2>/dev/null | tail -n 1 || true)
  if [ -n "$http" ]; then
    echo "answered $http"
  elif [ -n "$attempt_status" ]; then
    echo "exited $attempt_status, printing no HTTP status"
  else
    echo "left no exit status on record, printing no HTTP status"
  fi
}

[ "$#" -ge 1 ] || usage "$@"
mode=$1

case "$mode" in
  jobs)
    [ "$#" -eq 2 ] || usage "$@"
    out_file=$2
    need GH_TOKEN GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_RUN_ATTEMPT

    # This run's own attempt, which is the attempt this job is reporting on.
    route="repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"
    route="$route/attempts/$GITHUB_RUN_ATTEMPT/jobs?per_page=100"

    pages=$(mktemp)
    # One stderr file and one exit status per attempt, and a count of the attempts the ladder
    # made. gh_read runs its three attempts itself and sends every attempt's stderr straight
    # to this script's, with nothing between them, so one capture file cannot be taken apart
    # again afterwards: this wrapper is what keeps the attempts apart. It counts the attempt,
    # gives gh that attempt's own stderr file, writes that attempt's exit status beside it and
    # then puts the stderr file back on stderr, so the job log still holds every attempt's
    # words in order, as the inline read's did, and each attempt is still bounded by
    # `timeout 30`, which the ladder itself does not bound. The status is kept because the
    # bound firing leaves no word of its own in gh's stderr - the attempt exits 124 - and the
    # refusal line below has no other way to tell that case from a refusal gh printed no
    # status for.
    work=$(mktemp -d)
    trap 'rm -rf "$pages" "$work"' EXIT
    printf '0' > "$work/attempts"
    set +e
    gh_read bash -c '
      attempt=$(( $(cat "$1/attempts") + 1 ))
      printf "%s" "$attempt" > "$1/attempts"
      timeout 30 gh api --paginate --slurp "$2" 2> "$1/stderr.$attempt"
      status=$?
      printf "%s" "$status" > "$1/status.$attempt"
      cat "$1/stderr.$attempt" >&2
      exit "$status"
    ' bash "$work" "$route" > "$pages"
    read_status=$?
    set -e
    attempts=$(cat "$work/attempts")

    if [ "$read_status" -ne 0 ]; then
      # Every attempt is described from its own two files, and the line attributes each
      # description to the attempt it was read from. Two rounds of this lane's own fault are
      # why. Until 2026-11-08 one file held every attempt's stderr and the last match in it
      # won, which is an earlier attempt's status the moment two attempts are refused for
      # different reasons: an attempt answering `gh: Server Error (HTTP 502)` followed by two
      # answering `error connecting to api.github.com`, which carries no status, produced a
      # line naming HTTP 502 - a status that decided nothing and that sends a reader to
      # GitHub's API health. The repair of that, read on 2026-11-08, then said "no answer
      # within the 30 s bound on each attempt" from the deciding attempt's exit status alone,
      # which is the same fault with a bound in place of a status: two attempts refused with
      # a 502 and a third ended by the bound were all reported as having hit the bound.
      #
      # So the line names the attempt that decided the failure - the last one the ladder made
      # - and what that attempt did, and names an earlier attempt only where that attempt did
      # something else, each with its own number. Where every attempt ended the same way the
      # earlier ones are left unsaid: the deciding attempt's words already cover them, and a
      # list of three identical clauses tells a reader nothing the count does not.
      #
      # Within one attempt the exit status is read before the stderr, and the one shape that
      # tells this order from the other is an attempt that printed a status and then hung:
      # this order reports the 30 s bound, which is what ended that attempt, where reading the
      # stderr first would report the status the attempt had printed before it stopped
      # answering. An attempt that hung without printing anything reads as the bound in either
      # order, its stderr holding no status to find, so the order is not what keeps a silent
      # hang out of the third branch: the 124 check is, wherever it sits.
      #
      # With the command this script runs, an attempt that printed a status and then hung is
      # not a shape the lane has seen: `gh api --paginate` stops at the first page it is
      # refused on, so a page that printed a status ends the call rather than being followed
      # by a page that can hang, and the refusals measured 2026-11-08 on run 42468236584 -
      # HTTP 502 on pages 2 and 3 of the three-page `filter=all` read - were answered by
      # GitHub rather than left hanging. Not seen is not impossible, and nothing here rules it
      # out: such an attempt is reported as the bound and its status is not reported at all.
      # No test fixes this order either - measured 2026-11-08, a copy reading the stderr first
      # and the 124 second reads 135 passed, 0 failed - so what holds it is this paragraph.
      deciding=$(describe_attempt "$work" "$attempts")
      earlier=""
      for ((n = 1; n < attempts; n++)); do
        what=$(describe_attempt "$work" "$n")
        if [ "$what" != "$deciding" ]; then
          if [ -z "$earlier" ]; then
            earlier="where attempt $n $what"
          else
            earlier="$earlier and attempt $n $what"
          fi
        fi
      done
      [ -z "$earlier" ] || earlier=", $earlier"
      echo "::error::this run's jobs could not be read, so the legs this lane owed are" \
        "unknown: of GET $route, attempt $attempts, which decided the failure," \
        "$deciding$earlier; the ladder made $attempts attempts, all on run attempt" \
        "$GITHUB_RUN_ATTEMPT. This is a failed read of the jobs API and not a failed browser" \
        "row: no row of the suite failed here, and re-running this job reads the list again."
      exit 1
    fi

    # Each job's leg name is derived here, once, beside the fields it travels with, so the two
    # readings take it rather than each parsing the job name again.
    #
    # `shard (3)` becomes `shard-3` and `alone (webkit-alone)` becomes `alone-webkit-alone`,
    # after either caller's prefix. `merge` and `multiprocess` are this workflow's other two
    # jobs: they pass the name filter and get a null leg, so reading one passes them over and
    # reading three names them as holding no row. ci.yaml's jobs do not get a null leg - the
    # filter drops them first, which is what keeps them out of reading three, since that
    # reading selects on conclusion alone. The two that come nearest are ci.yaml's own
    # `dispatch-multiprocess` and `broker-multiprocess`, kept out only because the character
    # before `multiprocess` is a hyphen and the filter wants a space or a line start.
    #
    # The project part takes any name that holds no `)`, and the readings take one leg per
    # line, so a project named with a space, a digit or a capital is one leg here rather than
    # two or none.
    jq '[.[].jobs[] | select(.name | test("(^| )(shard|alone|multiprocess|merge)( \\(|$)"))]
      | map({id, name, conclusion,
          leg: ((.name | capture(" shard \\((?<n>[0-9]+)\\)$") | "shard-\(.n)")
            // (.name | capture(" alone \\((?<p>[^)]*)\\)$") | "alone-\(.p)")
            // null)})' "$pages" > "$out_file"
    ;;
  blobs-refused)
    [ "$#" -eq 1 ] || usage "$@"
    # No GH_TOKEN, no repository and no run id: this mode makes no request at all and names no
    # route, so the only thing it has to say is which step failed and on which attempt. What
    # the download action did - which call it made, whether anything was retried - is in that
    # step's own error line, which this process cannot read; the head says the rest.
    need GITHUB_RUN_ATTEMPT
    echo "::error::the legs' blob report artifacts could not be downloaded: the step" \
      "\"Download every leg's blob report\" failed on run attempt $GITHUB_RUN_ATTEMPT, and" \
      "that step's own error line in this job's log says why. This is a failed read of the" \
      "run's artifacts and not a failed browser row: no row of the suite failed here."
    ;;
  *)
    usage "$@"
    ;;
esac
