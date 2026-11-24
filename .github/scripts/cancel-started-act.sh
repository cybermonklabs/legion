#!/usr/bin/env bash
# The merge-group runs the queue has replaced, force-cancelled: this reads the run list of
# ci.yaml's merge_group runs, hands it to .github/scripts/merge-group-cancels.sh, which decides
# and prints one `cancel <run id>` line per run in the order to act on them, and makes one
# force-cancel call per line. A call that fails leaves a warning and not a red job: the run it
# names may have completed between the list and the call.
#
# Why a read this act could not make is no failure of the run it runs in. The act reclaims runner
# time and decides nothing: the run it runs in tests its own group whatever the act cancels, and a
# group left alone runs to its own end or is cancelled by the next group of the same pull request.
# Before this act said so, merge-group run 45467602844 (pull request #5331's second group) went
# red whole because the run list answered HTTP 500 once, in job 138706588845. So the list goes
# through gh-read.sh's gh_read, which calls three times, and a refusal still standing after those
# three warns and exits 0 where gh_read_transient reads it as GitHub's own: a 5xx, a 429, or a
# rate limit gh-read.sh could not wait out within the job's limit. Every other refusal fails the
# job as it did: a 4xx that is not 429 names a token or a route this job has wrong, which no later
# run mends by itself, and a list merge-group-cancels.sh cannot read fails in that script.
#
# Who runs it: the second of the two steps of .github/workflows/ci.yaml's cancel-started job,
# after its checkout. The job's `# why:` line names the notes section that says what that act is
# for and the rules the job follows. It takes no argument and reads three variables that step's
# own `env:` map sets: GH_TOKEN, the run's own token, for `gh`; and HEAD_BRANCH and RUN_ID, this
# run's queue branch and id, which merge-group-cancels.sh reads beside the RUNS this script
# exports. That job checks out with `sparse-checkout: .github`, so this script and the one it runs
# are both in the checkout when the step runs.
# The runner sets $GITHUB_REPOSITORY, which both `gh` calls name, for every step. Both calls go
# through gh-read.sh's gh_wait_reset, which waits out a spent API budget within the step's
# JOB_TIMEOUT_MINUTES and fails by name past it: the list is read through the gh_read ladder over
# it, and a refused cancel warns as it did.
#
# Run from the repository root, with those three and $GITHUB_REPOSITORY set:
#   .github/scripts/cancel-started-act.sh
# CI runs it in ci.yaml's cancel-started job, at its second step, after the checkout.
# Its tests are in cancel-acts.test.sh, which ci.yaml's test job runs.
set -euo pipefail
# shellcheck source=.github/scripts/gh-read.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/gh-read.sh"

# The words gh printed for the attempt the read ended on: gh_read hands them to this file rather
# than to the log, so the decision below can read the status out of them, and the act prints them
# to the log itself.
words=$(mktemp)
trap 'rm -f "$words"' EXIT

# The `--limit` is 100, well above the number of groups main's merge queue builds at once, and
# this pull request's own groups are among those: gh's default of 20 runs could leave an older
# group of it out of the list, and so out of the act.
listed=0
RUNS=$(gh_read --stderr "$words" gh run list --repo "$GITHUB_REPOSITORY" --workflow ci.yaml \
  --event merge_group --limit 100 --json databaseId,headBranch,status,createdAt) || listed=$?
cat "$words" >&2
if [ "$listed" -ne 0 ]; then
  route="GET repos/$GITHUB_REPOSITORY/actions/workflows/ci.yaml/runs, the route gh run list --workflow ci.yaml --event merge_group calls"
  if gh_read_transient "$listed" "$words"; then
    echo "::warning::GitHub refused $route, at every attempt gh-read.sh made ($(head -n 1 "$words")), so this run cancels none of the groups the queue has replaced and they run to their own end. The act reclaims runner time and decides nothing, so a list this run could not read is no failure of it."
    exit 0
  fi
  echo "::error::GitHub refused $route, at every attempt gh-read.sh made, and for no rate limit and no 5xx ($(head -n 1 "$words")): this job's token or route is wrong, which no later run mends" >&2
  exit 1
fi
export RUNS
acts=$(.github/scripts/merge-group-cancels.sh)
while read -r act id; do
  [ "$act" = cancel ] || continue
  gh_wait_reset gh api --silent --method POST \
    "repos/$GITHUB_REPOSITORY/actions/runs/$id/force-cancel" ||
    echo "::warning::force-cancel of run $id failed; it may have completed already"
done <<<"$acts"
