#!/usr/bin/env bash
# The one reading of whether a job's `failure` was its runner being shut down under it, so the
# two scripts that judge a run read it the same way: ci-ok.sh, which takes every job's result
# from `toJSON(needs)` and reads such a job as cancelled, and ci-ok-report.sh, which posts no
# early failing ci-ok when that is all that decided a merge-group run.
#
# What a runner shutdown is. GitHub takes the machine away under a running job — a hosted runner
# reclaimed, a runner service stopped — and the job's log ends with these two `##[error]` lines,
# which GitHub files as annotations on that job's check run:
#
#   The runner has received a shutdown signal. This can happen when the runner service is
#   stopped, or a manually started runner is canceled.
#   Process completed with exit code 143.
#
# 143 is 128 + SIGTERM, the status of a step the runner killed. The pair was read whole on
# theorem-labs/agent-infra run 42466562410, job `services (legion)` 128687417206, on 2026-11-08
# (dispatch://INFRA-49/comment/4083a833-8bb4-4dba-9d61-8f20757ca82c). The job's result is
# `failure`, the same result a test of the change failing gives it, so the result alone cannot
# tell a platform kill from a fault of the change and those two annotations are what does.
#
# What it reads, and from where. Both reads go through gh-read.sh's ladder, which this file
# sources, so one refusal decides nothing:
#   GET repos/{repo}/commits/{HEAD_SHA}/check-runs?filter=all&per_page=100
#     `name`, `url` and `details_url` of every check run on this run's commit. A job's own check
#     run is GitHub's, made when the job starts, and its `url` is the one this script reads
#     annotations from. This is the route ci-ok-reconcile.sh:96 already reads from the ci-ok
#     job, and it is read here rather than the run's jobs route — which hands the same mapping
#     as `check_run_url` — because this route needs only a scope the reading job already
#     declares: the ci-ok job's `permissions:` block gives it `checks: write`, which carries
#     the `checks: read` this route wants, and so does ci-ok-report.sh's caller (the scope
#     probe, run 43164736741). It is not that the jobs route is out of reach: probe #3770 ran
#     a step inside the ci-ok job itself on run 43168566331 and printed `INFRA-1413 cause
#     probe: jobs-anonymous=200 jobs-with-token=200 secrets-with-token=403`, so the jobs route
#     answers from that job with no token at all — this repository is public, exactly as
#     ci.yaml:121-123 already records for the `changes` job's read of the pull request's file
#     list — while the `403` on the secrets route is the control showing that same token is
#     restricted. What reading the check runs buys is that this reading rests on a scope the
#     job declares rather than on the repository staying public. `filter=all` because the
#     default, `latest`, keeps one check run per name, and a head can carry an earlier
#     attempt's as well.
#   GET {url}/annotations?per_page=100
#     `annotation_level` and `message` of every annotation of that check run. `checks: read`,
#     which the `checks: write` of both callers' jobs carries.
#
# Which check runs are this run's. The commit carries every workflow's, so the run id is what
# narrows them: a job's check run has `details_url` `<server>/<repo>/actions/runs/<run id>/job/
# <job id>`, and this script keeps those whose `details_url` holds `/actions/runs/{GITHUB_RUN_ID}
# /job/`. Within them a job is every check run whose name is the job's id in ci.yaml or starts
# with `<job> / `, which is how ci-ok-report.sh's read of the jobs route takes a called
# workflow's jobs too.
#
# A job with no check run of this run on that commit — a job GitHub has not started, a name no
# check run carries — is read as no kill at all, and so is a job whose check runs carry no
# failure annotation: its failure stands as a failure. Nothing this script cannot find is
# evidence that the platform killed anything, and a verdict that passed on a read it could not
# make would let a real failure through.
#
# A job is read as shut down when at least one of its check runs carries a failure-level
# annotation and every check run of it that carries one carries both facts: a message holding
# `has received a shutdown signal` and a message holding `exit code 143`. So a job whose only
# failure annotation is its own error stays a failure, a job that carries the shutdown message
# with another exit code stays a failure, and a called workflow whose one job failed of itself
# beside another the platform killed stays a failure.
#
# Why the annotations and not the job's `conclusion`: ci-ok-report.sh asks about the job it runs
# in, which has not ended while that step runs (its header says the jobs route shows it
# `in_progress`), so a reading keyed on a conclusion would answer nothing for the one job that
# decides a merge-group run. The annotations are on the check run as soon as the log line is,
# whatever the job's status.
#
# Sourced, never run: `source "$(dirname "${BASH_SOURCE[0]}")/runner-shutdown.sh"`.
#
# Its inputs, from the caller's environment:
#   GH_TOKEN           the job's github.token, with `checks: write` (or `checks: read`).
#   HEAD_SHA           the commit this run's check runs are on: a pull request's head or
#                      github.sha, the expression both callers' steps already pass.
#   GITHUB_REPOSITORY, GITHUB_RUN_ID, as Actions sets them: the run whose check runs are read.
#   GH_RETRY_SECONDS   optional: gh-read.sh's knob, the tests' way to retry without waiting.
#
# runner_shutdown_kills <job>...: prints those of the named jobs this run's data shows a runner
# shutdown killed, one per line, and returns 0. It reads the commit's check runs on its first
# question and keeps them in a variable for the rest of that shell, but both callers take its
# output through `$(…)`, which is a subshell: ci-ok.sh:124 in one call naming every failed job,
# ci-ok-report.sh:122 one job per call. The variable dies with the subshell, so a call made
# that way reads the route again and the keeping saves nothing across such calls — asking job
# by job costs one read of the check runs per job. It returns 1, having warned, when GitHub
# refused one of its reads three times or the environment above is incomplete: the caller then
# keeps every failure a failure and the verdict stays the failing one, which is how
# ci-ok-report.sh:196-201 answers a refused jobs list too. Every warning goes to stderr, where
# the job log holds it as gh-read.sh's own attempts are held, so a caller may read the job
# names off this function's stdout.

# shellcheck source=.github/scripts/gh-read.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/gh-read.sh"

# This run's check runs, read on the first question and kept for the rest of the shell that
# asks: a caller's `$(…)` is a subshell of its own, so what it keeps dies with that call.
_runner_shutdown_check_runs=

# _runner_shutdown_other_exit_code <annotations>: returns 0 when these failure annotations of one
# check run name an exit code that is not 143. That is the job's own `Process completed with exit
# code 1.`, which sits beside the runner's two lines whenever a step failed first and the shutdown
# caught a later step — every job of ci.yaml runs its ci-ok-report step under `if: always()`, so
# a failed job keeps running after its fault. An annotation that names no exit code at all is no
# evidence either way and refuses nothing: the shutdown message is itself such an annotation, and
# refusing on it would leave no kill readable.
_runner_shutdown_other_exit_code() {
  local rest=$1
  while [[ $rest =~ exit\ code\ ([0-9]+) ]]; do
    [ "${BASH_REMATCH[1]}" = 143 ] || return 0
    rest=${rest#*"exit code ${BASH_REMATCH[1]}"}
  done
  return 1
}

runner_shutdown_kills() {
  local input job urls url annotations shutdown
  local kills=()
  [ "$#" -gt 0 ] || return 0
  for input in GH_TOKEN HEAD_SHA GITHUB_REPOSITORY GITHUB_RUN_ID; do
    if [ -z "${!input:-}" ]; then
      echo "::warning::runner-shutdown.sh needs $input in its environment to tell a runner shutdown from a failure of the change; every failure stands as a failure" >&2
      return 1
    fi
  done
  if [ -z "$_runner_shutdown_check_runs" ]; then
    if ! _runner_shutdown_check_runs=$(gh_read gh api --paginate \
      "repos/$GITHUB_REPOSITORY/commits/$HEAD_SHA/check-runs?filter=all&per_page=100" \
      --jq '.check_runs[] | {name, url, details_url}'); then
      echo "::warning::GitHub refused the check runs on $HEAD_SHA three times, so no failure of this run can be read as a runner shutdown; every failure stands as a failure" >&2
      return 1
    fi
    _runner_shutdown_check_runs=$(jq -s --arg run "/actions/runs/$GITHUB_RUN_ID/job/" \
      'map(select((.details_url // "") | contains($run)))' <<<"$_runner_shutdown_check_runs")
  fi
  for job in "$@"; do
    urls=$(jq -r --arg job "$job" \
      '.[] | select(.name == $job or (.name | startswith($job + " / "))) | .url' \
      <<<"$_runner_shutdown_check_runs")
    [ -n "$urls" ] || continue
    shutdown=false
    while read -r url; do
      if ! annotations=$(gh_read gh api --paginate "$url/annotations?per_page=100" \
        --jq '.[] | select(.annotation_level == "failure") | .message'); then
        echo "::warning::GitHub refused the annotations of $job three times, so its failure cannot be read as a runner shutdown; it stands as a failure" >&2
        return 1
      fi
      # A check run with no failure annotation is a part of the job that did not fail; one that
      # has them carried the kill only when it holds both facts and no annotation of it names
      # another exit code, or the job failed of itself.
      [ -n "$annotations" ] || continue
      if [[ $annotations == *"has received a shutdown signal"* && $annotations == *"exit code 143"* ]] \
        && ! _runner_shutdown_other_exit_code "$annotations"; then
        shutdown=true
      else
        shutdown=false
        break
      fi
    done <<<"$urls"
    if [ "$shutdown" = true ]; then
      kills+=("$job")
    fi
  done
  if [ "${#kills[@]}" -gt 0 ]; then
    printf '%s\n' "${kills[@]}"
  fi
}
