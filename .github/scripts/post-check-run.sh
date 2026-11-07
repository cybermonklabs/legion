#!/usr/bin/env bash
# Posts one completed check run on a commit as this workflow's GITHUB_TOKEN, which is the GitHub
# Actions App (integration 15368, the source main's ruleset names for its required checks): the
# way ci-ok-report.sh reports the failing ci-ok of a merge-group run one job's result has already
# decided, from a step of a job that already holds a runner. It is the one script here that posts
# a check run, so the measurement that decides what may be posted at all is kept whole in this
# header and the callers point at it.
#
# A check run POSTed with POST /repos/.../check-runs joins the Actions App's first check suite at
# that commit rather than the posting run's, measured four times: on probe branch
# probe/infra1066-ciok at 365e2766, where the posting job ran in suite 114285918850 and the check
# run it created landed in 114285918626, the other workflow's; on pull request #2860 at 57182d42,
# where CI's ci-ok landed in the suite of "Rebuild Claude Envoy plugin" (114301219205) rather
# than in CI's own (114301219822); on #2847, where it landed in PR Title's suite (114222733751
# against CI's 114222734375, created in the same second), the rollup reported the required ci-ok
# under workflow "PR Title" and the merge queue refused to enqueue with `Required status check
# "ci-ok" is expected` while that ci-ok was green; and at head 44af468c, where the pr-title lint
# posted landed in the suite of "Fold Changelog Fragments" (check run 127894229103, suite
# 114391379333). So no check whose suite must be its run's own is posted here: both checks that
# ruleset requires are jobs of ci.yaml, ci-ok and pr-title, and a job's own check run is in its
# run's own suite by construction. The job needs `checks: write`.
#
# Its inputs, from the step's env:
#   GH_TOKEN           the job's github.token.
#   GITHUB_REPOSITORY  owner/repo, as Actions sets it.
#   HEAD_SHA           the commit the check is for: a pull request's head (not the merge commit a
#                      pull_request run checks out, which no ruleset reads) or the pushed commit.
#   CHECK_NAME         the check's name, exactly as the ruleset requires it.
#   CONCLUSION         success or failure.
#   TITLE              one line, shown beside the check.
#   SUMMARY_FILE       a file whose text is the check's summary (markdown), cut to GitHub's limit.
#   DETAILS_URL        where the check links: the job that decided it.
#   EXTERNAL_ID        the posted check run's `external_id`: the one field that tells a check run
#                      posted here from a job's own check run of the same name, which
#                      ci-ok-reconcile.sh reads to decide which ci-ok it may rewrite. It has
#                      three shapes. A `ci-ok-post:<the posting run's id>` prefix is a post of
#                      this release, which every ci-ok posted here carries. An empty field is a
#                      post of the release before that marker existed. A UUID is a job's own
#                      check run, which GitHub writes and GitHub alone concludes: measured on
#                      both job check runs of INFRA-1066's k28 probe head and on
#                      4a53aeee-0d45-5854-8639-11c4a3f4d78f, the ci-ok job's own check run
#                      127905818541 of the run that read them (2026-11-06). So the test is the
#                      field's shape: a prefix or an empty field is a post, and a UUID is a job's
#                      own check run.
#
# Run from anywhere: HEAD_SHA=... CHECK_NAME=... CONCLUSION=... TITLE=... SUMMARY_FILE=...
#   DETAILS_URL=... EXTERNAL_ID=... .github/scripts/post-check-run.sh
# Its tests are in ci-ok-report.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/ci-retry.sh
. "$script_dir/ci-retry.sh"

for input in GH_TOKEN GITHUB_REPOSITORY HEAD_SHA CHECK_NAME CONCLUSION TITLE SUMMARY_FILE DETAILS_URL EXTERNAL_ID; do
  if [ -z "${!input:-}" ]; then
    echo "::error::post-check-run.sh needs $input in its environment"
    exit 1
  fi
done
case $CONCLUSION in
  success | failure) ;;
  *)
    echo "::error::post-check-run.sh posts success or failure, not '$CONCLUSION'"
    exit 1
    ;;
esac

# GitHub refuses a summary longer than 65535 characters. The body goes to a file rather than to
# gh's stdin: a retried post must send it again, and the five attempts of
# .github/scripts/ci-retry.sh share the one stdin a here-string gives them, which nothing
# rewinds. Measured under bash 5.2 over a retry ladder of that shape: a refused attempt that
# read nothing leaves the whole body for the next attempt, one that read the body to its end
# leaves it nothing, and one refused after 40 of 121 bytes leaves the other 81 — so the body a
# retry sent would be whatever the refusal before it happened to leave. `--input` names a file,
# which every attempt opens and reads in full.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
body="$work/body.json"
jq -n --arg name "$CHECK_NAME" --arg sha "$HEAD_SHA" --arg conclusion "$CONCLUSION" \
  --arg url "$DETAILS_URL" --arg title "$TITLE" --arg external_id "$EXTERNAL_ID" \
  --rawfile summary "$SUMMARY_FILE" \
  '{name: $name, head_sha: $sha, status: "completed", conclusion: $conclusion, details_url: $url,
    external_id: $external_id,
    output: {title: $title, summary: ($summary | .[0:65000])}}' >"$body"
# The post retries: .github/scripts/ci-retry.sh makes five bounded attempts, the last starting
# 150 s after the first, and an exhausted post fails the step. The loop that stood here, and the
# second ladder the ci-ok scripts kept beside it, were each a copy of that helper, which is what
# dispatch://INFRA-1545's R2 refuses. gh writes the posted check's URL into a file through
# `sh -c`, so that each attempt starts that file over, for the reason ci-ok-report.sh gives
# where it does the same.
#
# 30 s per attempt: the bound that file derives there for a JSON call on one REST route, about
# 3 s of work, which this post is no larger than.
# shellcheck disable=SC2016 # $1 to $4 are sh -c's own arguments, expanded by that shell
if retry_fetch "the $CHECK_NAME check run" 30 -- \
  sh -c 'gh api --method POST "$1" --input "$2" --jq "$3" > "$4"' gh-api \
  "repos/$GITHUB_REPOSITORY/check-runs" "$body" '.html_url' "$work/posted"; then
  echo "posted $CHECK_NAME ($CONCLUSION) on $HEAD_SHA: $(<"$work/posted")"
  exit 0
fi
echo "::error::GitHub refused the $CHECK_NAME check run on $HEAD_SHA five times"
exit 1
