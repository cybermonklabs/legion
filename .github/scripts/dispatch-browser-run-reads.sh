#!/usr/bin/env bash
# The reads of its own run that the merge job of .github/workflows/dispatch-browser.yaml
# makes, in one file, so that a test can drive them under a stand-in `gh` rather than on a
# runner against GitHub.
#
# Three modes, after dispatch-browser-samples.sh's two:
#
#   .github/scripts/dispatch-browser-run-reads.sh jobs <out-file>
#   .github/scripts/dispatch-browser-run-reads.sh blobs-refused
#   .github/scripts/dispatch-browser-run-reads.sh machine-time <jobs-file> <lane>
#
# `jobs` reads this run's own attempt's jobs and writes <out-file>: the array of
# {id, name, conclusion, started_at, completed_at, leg} that the merge job's readings consume -
# reading one for the legs that owed a blob report, reading three for the legs that did not
# succeed, and reading five for the machine time this run has cost.
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
# `machine-time` makes no request either: it reads the file `jobs` wrote, sums the seconds of
# every job of this workflow that concluded with a duration, this merge job and every skipped
# job apart, and fails the job above the lane's budget. Its own branch below carries the
# budgets, where they came from and why they are lower than R5's own bounds.
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
# GITHUB_RUN_ID and GITHUB_RUN_ATTEMPT, as Actions sets them and as the step "Reading one, the
# rows" sets GH_TOKEN in its `env:`, a key ci_filter_coverage.py's ENV_ALLOWED holds - and never
# from an argument, which every process of the runner can read on the command line.
#
# Run from anywhere: .github/scripts/dispatch-browser-run-reads.sh jobs <out-file>,
# .github/scripts/dispatch-browser-run-reads.sh blobs-refused, or
# .github/scripts/dispatch-browser-run-reads.sh machine-time <jobs-file> <lane>
# CI runs it in dispatch-browser.yaml's merge job, at the step "Reading one, the rows" for the
# `jobs` mode, at the step "Report a refused blob download as a refused read" for
# `blobs-refused` and at the step "Reading five, the lane's machine time" for `machine-time`,
# and its tests in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/gh-read.sh
source "$script_dir/gh-read.sh"

usage() {
  echo "::error::dispatch-browser-run-reads.sh takes a mode and its arguments:" \
    ".github/scripts/dispatch-browser-run-reads.sh jobs <out-file>, which writes this run's" \
    "attempt's jobs, .github/scripts/dispatch-browser-run-reads.sh blobs-refused, which" \
    "prints the line for a blob download that could not be completed, or" \
    ".github/scripts/dispatch-browser-run-reads.sh machine-time <jobs-file> <lane>, which" \
    "holds this run's machine time to its lane's budget. It was given: $*"
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
# nothing, and the words state only what the two files do not hold: no exit status on record,
# which is `status.<n>` unwritten, and no HTTP status printed, which is `stderr.<n>` holding
# none. They name neither file, and they do not say the attempt did not exit: an attempt that
# reaches this branch may well have exited. Measured 2026-11-08, with the wrapper of the third
# of three attempts killed, that wrapper exited 137 and 137 is the status the ladder's attempt
# saw, while nothing had written `status.3`.
#
# What keeps a missing `status.<n>` from ending this read is the `|| true` on the `cat`, not
# the `2>/dev/null` beside it, which only keeps `cat: <dir>/status.3: No such file or
# directory` out of the job log. The `|| true` on the `grep` covers something else: under
# `pipefail` a `grep` that matches nothing exits 1, which is any attempt whose stderr holds no
# `HTTP` status. Measured 2026-11-08 against the test file beside this one: the `cat`'s
# `|| true` taken off alone fails 8 checks, four in each of the two cases that stage a killed
# wrapper; the `grep`'s taken off alone fails 12, eight of them in three cases where every
# file exists; and both `2>/dev/null` taken off fail none, where a by-hand drive of a killed
# wrapper against that last copy prints that one line beside the refusal line and the suite
# prints neither.
#
# Two shapes reach the fourth branch. One is a wrapper killed during an attempt: the
# redirection has made `stderr.<n>` and nothing has written `status.<n>`, and where gh had
# printed a status on that stderr before the wrapper died, the attempt is described from it
# rather than by these words. The test file's case "a killed wrapper whose gh had printed a
# status before it died" is what holds that order: a copy asking whether the status file is
# missing before it greps the stderr fails 2 checks, both in that case, measured 2026-11-08. The
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
      # and the 124 second fails no check - so what holds it is this paragraph.
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
    # `shard (2)` becomes `shard-2` and the `alone` job, which has no matrix and so no brackets,
    # becomes `alone`. Both are read with either caller's prefix (`dispatch-browser / shard (2)`,
    # as ci.yaml and the nightly name a called workflow's job) and with none at all: the two
    # shapes must read the same leg, because a leg absent from reading one's list is a leg whose
    # missing blob nothing reports, and the shard capture required the leading space while the
    # `alone` test did not (dispatch://INFRA-1109). `binaries`, `merge`, `multiprocess`,
    # `embedder` and `visual` are this workflow's other five jobs: they pass the name filter and
    # get a null leg, so reading one passes them over and reading three names them as holding no
    # row. `binaries` is in that filter because every other job of the workflow needs it, so a
    # build that failed leaves all five skipped and no blob report anywhere, and a run read
    # without it is reported as legs that uploaded nothing rather than as the build that handed
    # them nothing. `visual` is in it because a run whose only red job is that one must not read
    # "every leg of this workflow concluded success": the verdict step fails ci-ok either way, so
    # the gate held, but the summary a person reads was wrong. `embedder` is there for the same
    # reason. ci.yaml's jobs do not get a null leg - the filter drops them first, which is what
    # keeps them out of reading three, since that reading selects on conclusion alone. The two
    # that come nearest are ci.yaml's own `dispatch-multiprocess` and `broker-multiprocess`,
    # kept out only because the character before `multiprocess` is a hyphen and the filter wants
    # a space or a line start.
    #
    # The alone job ran a matrix of three projects until dispatch://INFRA-1109 put them on one
    # machine, and its leg was read out of the brackets after the job name
    # (`alone (webkit-alone)` -> `alone-webkit-alone`). There is one such job now and it writes
    # one blob report, `report-alone.zip`, and one `rows/alone.txt`, so the leg is the bare word
    # and nothing is parsed out of brackets for it. A leg name is still read one per line by
    # both readings, so a name holding a space would be one leg rather than two.
    #
    # `started_at` and `completed_at` travel with those four because the `machine-time` mode
    # below sums them and the jobs answer already carries both: adding them to this filter
    # costs no second request and no second route. They are the API's own strings, kept as
    # they are answered (`2026-11-11T10:22:27Z`) rather than turned into seconds here, so a
    # reader of jobs.json sees what GitHub said; `completed_at` is null for a job that is
    # still running, which this job itself is while it writes this file.
    jq '[.[].jobs[] | select(.name |
        test("(^| )(binaries|shard|alone|multiprocess|embedder|visual|merge)( \\(|$)"))]
      | map({id, name, conclusion, started_at, completed_at,
          leg: ((.name | capture("(^| )shard \\((?<n>[0-9]+)\\)$") | "shard-\(.n)")
            // (if (.name | test("(^| )alone$")) then "alone" else null end))})' \
      "$pages" > "$out_file"
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
  machine-time)
    [ "$#" -eq 3 ] || usage "$@"
    jobs_file=$2
    lane=$3
    # THE LANE'S MACHINE TIME, AND WHY A RUN REFUSES ITSELF OVER IT.
    #
    # dispatch://INFRA-1109 cut this suite from 45 to 49 runner-minutes a full run to 35 to 40,
    # and the reason it had grown that far is that nothing in the tree refused the growth: the
    # suite cost about 25 runner-minutes as two jobs on 2026-11-07 (run 42406948509) and 45 to
    # 49 over twelve jobs on 2026-11-10 (run 42809067358, 2,945 s), and no check saw it between
    # those two days. This mode is that check (R12 of dispatch://INFRA-1109/spec). It is read
    # inside the run, by the job that already publishes the run's report, out of the jobs
    # answer that job already holds - .github/workflows/dispatch-browser.yaml's merge job
    # writes $RUNNER_TEMP/jobs.json at "Reading one, the rows" and this reading takes that file
    # again - so the budget costs no request of its own.
    #
    # WHAT IT SUMS. Every job of this workflow that has concluded, this merge job apart.
    #
    # - This job is left out because it cannot read its own seconds: its `completed_at` is null
    #   while it runs, which is the same null a job still running has, and a sum that waited
    #   for it would never be taken at all.
    # - A skipped job is left out because it costs no machine time and the API gives it a
    #   negative span: on run 43002633713, the pull-request lane, `dispatch-browser / alone` is
    #   `skipped` with `started_at 2026-11-11T10:22:27Z` and `completed_at 2026-11-11T10:22:26Z`
    #   (read 2026-11-11). Counting that would subtract a second from the lane's cost.
    # - A job with no `completed_at` is left out for the same reason this job is: it has not
    #   finished, so it has no duration yet. A job of this workflow still running when the
    #   merge job reports is one the merge job did not wait for, which no job of this file is
    #   today: every one of them is in the merge job's `needs:`.
    #
    # THE BOUND, AND WHAT IT IS NOT. R5 of dispatch://INFRA-1109/spec holds a full lane to four
    # fifths of the median of that day's green default-branch full-lane runs, and a
    # pull-request lane to 900 s. No job can read another run, so this reading cannot be that
    # one: it is a coarse absolute ceiling above the fleet's own spread, and it holds nothing
    # this issue delivers. What this issue delivers is read after the run, against the day's
    # base: R5's four fifths and R4's machine-seconds a row.
    #
    #   full lane:         3,200 - 44 = 3,156 s
    #   pull-request lane:   900 - 44 =   856 s
    #
    # The full lane's 3,156 s sits between the two limits such a ceiling has: its margin over
    # this tree is wider than the spread of the default branch's own green lanes, and it is below
    # a suite grown by half. Every figure of the three points below is a lane as this step reads
    # it, its merge job apart, from the jobs API.
    #
    # - It is 27 per cent above this tree's worst full-lane reading, 2,484 s (run 43885980414,
    #   at 61e5c402): 3,156 / 2,484 = 1.27. That margin is wider than the 14 per cent between
    #   the cheapest and the dearest of the 27 green default-branch full lanes of 2026-11-16
    #   (3,032 and 3,444 s), and wider than the 12 per cent between that day's median, 3,189 s,
    #   and the median of the 23 green ones of 2026-11-11, 2,845 s.
    # - It is below one and a half times this tree's median full-lane reading, 2,231 s (runs
    #   43877804247, 43885980414 and 43897620046 read 2,062, 2,484 and 2,231 s), which is
    #   3,347 s, so it refuses a suite grown by more than 41 per cent (3,156 / 2,231 = 1.41).
    # - It is below the median of the 2026-11-16 default-branch runs, 3,189 s, so it refuses
    #   that day's median run of the suite this issue replaced, but not its cheaper ones: 10 of
    #   those 27 read 3,032 to 3,146 s, and every one of the 23 of 2026-11-11 read under it. So
    #   this guard does not hold the saving. R5's four fifths of the same day's median, read
    #   after the merge (check 52463e11 of dispatch://INFRA-1109/artifact/verification-md), does.
    #
    # The full lane's bound was 2,040 s when this mode was written, from four runs of 2026-11-11
    # and 2026-11-12, 43045412941, 43074443876, 43079610268 and 43150379433, which cost 2,061,
    # 1,974, 1,997 and 2,041 s with their merge jobs (2,025, 1,946, 1,964 and 2,003 s without);
    # it became 2,400 s when the machines moved, not when this suite did - between run
    # 43150379433 (2,003 s) and run 43192333526 (2,285 s), both dispatched on 2026-11-12, the
    # binaries job, which runs no row, rose from 157 to 205 s - and 2,700 s when one slow pair of
    # runs proved that an absolute figure near the measurement is a figure the fleet decides. On
    # 2026-11-15 two runs of one tree cost 2,252 and 2,450 s with their merge jobs (43664675139,
    # 43667557013), whose real-stack row medians were 2,746 and 3,126 ms: the lane's cost tracked
    # the runner and not the rows. The default branch moved with it, as it always does - the
    # thirteen green merge-queue runs of that day up to 15:03Z cost 2,975 to 3,347 s with their
    # merge jobs, with a median of 3,126 s (.github/ci-notes/dispatch-browser.md's section on
    # what the job set costs) - which is why the ratio
    # is what R5 and R4 read and this ceiling is not. It became 3,200 s on 2026-11-17, when pull
    # request #3513's full lanes at 61e5c402 and 1ba866a9 read 2,062 to 2,484 s at this step,
    # the worst of them 6.5 per cent under 2,656 s, so a slow fleet or the rows the default
    # branch adds in a day could have stopped the merge queue with no regression in it (review
    # 5929818647 of pull request #3513).
    # The pull-request lane keeps 900 s, since the default branch runs no such lane and there
    # is no same-day figure to take a ratio against.
    # The 44 s is measured, not allowed for: it is the most the `dispatch-browser / merge` job
    # took over pull request #3513's fifteen full-lane runs and twenty-one pull-request runs of
    # 2026-11-15 up to 17:44Z, read from the jobs API - job 132733634671 of run 43687205491,
    # above the 39 s of job 132868945016 (run 43731214579) and the 38 s of runs 43672161759 and
    # 43656706589.
    # Every one of those jobs ran the step this mode is the body of, so the figure holds that
    # step's second. It replaces 37 s, which was 36 s measured over five runs of 2026-11-11,
    # before that step existed, plus 1 s allotted to the step; pull request #3513's own merge jobs
    # went past it. That job's timeout-minutes is 4, so a merge job above the whole figure and
    # not cancelled is one whose seconds this figure then understates, and this budget is the
    # looser for it by exactly the difference.
    # Subtracting the maximum rather than the median is what keeps this reading stricter than
    # the ceiling itself and never looser: a lane that passes here at 3,156 s and then spends a
    # worst-case 44 s in this job lands on 3,200 exactly.
    #
    # This reading is what refuses the growth before the run ends; what the run is held to
    # afterwards, against the day's own base, is R5's and R4's.
    #
    # WHY AN UNREADABLE LIST REFUSES. A budget that passes when it cannot read its number is
    # the defect this mode exists against, so a jobs file that is not there, that no job of
    # this lane has concluded in, or that jq cannot read is a refusal and names itself as one.
    #
    # Read it again over any run, offline, with no runner and no second request:
    #
    #   gh api repos/cybermonklabs/legion/actions/runs/<id>/attempts/1/jobs?per_page=100 \
    #     --paginate --slurp > pages.json
    #   .github/scripts/dispatch-browser-run-reads.sh jobs jobs.json   # over those pages
    #   .github/scripts/dispatch-browser-run-reads.sh machine-time jobs.json full
    case "$lane" in
      full)
        bound=3200
        ;;
      pull-request)
        bound=900
        ;;
      *)
        echo "::error::dispatch-browser-run-reads.sh machine-time takes the lane this run is," \
          "which is pull-request or full, not '$lane'"
        exit 1
        ;;
    esac
    # This job's own worst case, and the one subtraction both lanes' budgets are: a figure
    # changed here moves each budget with it, where a budget written out beside its bound could
    # leave this job's seconds counted twice or not at all.
    merge_maximum=44
    budget=$(( bound - merge_maximum ))
    if [ ! -f "$jobs_file" ]; then
      echo "::error::this run's machine time could not be summed: the jobs of this run are" \
        "read from $jobs_file and that file is not there, so the $lane lane was held to no" \
        "budget. The step that writes it says why it could not. This is a refused reading and" \
        "not a lane inside its budget."
      exit 1
    fi
    measured=$(mktemp)
    trap 'rm -f "$measured"' EXIT
    # One line per job the sum holds: its seconds and its name, longest first, which is the
    # order a person reading the summary wants. `fromdateiso8601` reads the API's own
    # `2026-11-11T10:22:27Z`, and a file holding anything else ends this call rather than
    # answering a sum of part of it.
    jq -r '[.[]
        | select(.conclusion != null and .conclusion != "skipped")
        | select(.started_at != null and .completed_at != null)
        | select(.name | test("(^| )merge$") | not)
        | {name, seconds: ((.completed_at | fromdateiso8601) - (.started_at | fromdateiso8601))}]
      | sort_by(-.seconds)[] | "\(.seconds)\t\(.name)"' "$jobs_file" > "$measured" || {
      echo "::error::this run's machine time could not be summed: $jobs_file is not a list of" \
        "this run's jobs that jq could read, so the $lane lane was held to no budget. This is" \
        "a refused reading and not a lane inside its budget."
      exit 1
    }
    # Both numbers in one pass, with the guard above the sum they are read for: a reading of no
    # job at all is refused rather than summed to zero.
    read -r count seconds <<<"$(awk -F'\t' '{ total += $1 } END { print NR, total + 0 }' \
      "$measured")"
    if [ "$count" -eq 0 ]; then
      echo "::error::no job of this $lane lane has concluded with a duration, so its machine" \
        "time could not be summed and the lane was held to no budget. This is a refused" \
        "reading and not a lane inside its budget."
      exit 1
    fi
    if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
      {
        echo "### The lane's machine time"
        echo
        echo "| job | seconds |"
        echo "| --- | ---: |"
        # One job per line, and the name last, where a job name holding a space is still one
        # name: the same rule reading one's leg loop follows.
        while IFS="$(printf '\t')" read -r job_seconds job_name; do
          echo "| $job_name | $job_seconds |"
        done < "$measured"
        echo
        echo "The $count concluded jobs of this $lane lane, this merge job apart, cost" \
          "$seconds s against a budget of $budget s, which is the committed ceiling of" \
          "$bound s less this job's own worst case of $merge_maximum s."
      } >> "$GITHUB_STEP_SUMMARY"
    fi
    if [ "$seconds" -gt "$budget" ]; then
      echo "::error::this $lane lane has cost $seconds s of machine time over its $count" \
        "concluded jobs, this merge job apart, which is above the $budget s they are held to:" \
        "the committed ceiling of $bound s for this lane, less this job's own worst case of" \
        "$merge_maximum s, which a reading taken inside the run cannot hold. The table in this" \
        "run's summary names each job's seconds. This is the suite costing more machine time" \
        "than dispatch://INFRA-1109 left it costing, and not a failed browser row."
      exit 1
    fi
    echo "::notice title=dispatch-browser machine time::the $lane lane cost $seconds s over" \
      "$count concluded jobs, this merge job apart, against a budget of $budget s ($bound s" \
      "less this job's own worst case of $merge_maximum s)"
    ;;
  *)
    usage "$@"
    ;;
esac
