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
# Why the route is the attempt endpoint and not `filter=all`. The merge job read
# `.../runs/<id>/jobs?filter=all&per_page=100`, which lists every attempt's jobs, so each re-run
# makes the read heavier: measured 2026-11-08 on run 42468236584, `filter=all` answers 276 jobs
# over three pages and GitHub answers HTTP 502 on pages 2 and 3, while
# `.../attempts/<n>/jobs?per_page=100` answers 46 jobs on one page for each of attempts 1 to 6.
# A job of this run reads its own attempt, which is the one it is reporting on, so there is no
# `filter=all` anywhere in this file.
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
# A read refused after the ladder names the endpoint, the status gh last printed and the run
# attempt, and says in those words that it is a failed read and not a failed row - the merge
# job's readings name legs, and a reader who meets one of these lines must not take it for one.
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
    # The number of attempts the ladder makes. It mirrors gh-read.sh's own `for attempt in 1 2
    # 3` loop, it is named only by the refusal line below, and it must move when that loop does.
    read_attempts=3

    pages=$(mktemp)
    read_stderr=$(mktemp)
    trap 'rm -f "$pages" "$read_stderr"' EXIT
    # gh's own output reaches the job log whether the read answered or was refused, as the
    # inline read's did; it is captured first only so that the refusal line below can read the
    # status out of it, and `cat` puts every attempt's back unchanged.
    set +e
    gh_read timeout 30 gh api --paginate --slurp "$route" > "$pages" 2> "$read_stderr"
    read_status=$?
    set -e
    cat "$read_stderr" >&2

    if [ "$read_status" -ne 0 ]; then
      # The last match, so the last attempt's status wins: the attempts before it were refused
      # for reasons of their own, and the one that decided the failure is the one to name. No
      # match at all is a refusal gh printed no status for - a resolution failure, a timeout -
      # and the line says that rather than naming a status that was never answered.
      http=$(grep -o 'HTTP [0-9][0-9][0-9]' "$read_stderr" | tail -n 1 || true)
      if [ -n "$http" ]; then
        answered="the last attempt answered $http"
      else
        answered="gh printed no HTTP status"
      fi
      echo "::error::this run's jobs could not be read, so the legs this lane owed are" \
        "unknown: GET $route was refused and $answered; the ladder made $read_attempts" \
        "attempts, all on run attempt $GITHUB_RUN_ATTEMPT. This is a failed read of the jobs" \
        "API and not a failed browser row: no row of the suite failed here, and re-running" \
        "this job reads the list again."
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
    # No GH_TOKEN: this mode makes no request at all, it only says what the download step that
    # already failed was doing.
    need GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_RUN_ATTEMPT
    echo "::error::the legs' blob report artifacts could not be downloaded: GET" \
      "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts, on run attempt" \
      "$GITHUB_RUN_ATTEMPT, did not answer actions/download-artifact, which exhausted its own" \
      "five retries. This is a failed read of the run's artifacts and not a failed browser" \
      "row: no row of the suite failed here, and the blobs the legs uploaded are still on the" \
      "run for a re-run of this job to take."
    ;;
  *)
    usage "$@"
    ;;
esac
