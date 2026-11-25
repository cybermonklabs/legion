#!/usr/bin/env bash
# The merge-group runs the queue has replaced, force-cancelled: this reads the run list of
# ci.yaml's merge_group runs, hands it to .github/scripts/merge-group-cancels.sh, which decides
# and prints one `cancel <run id>` line per run in the order to act on them, and makes one
# force-cancel call per line. A call that fails leaves a warning and not a red job: the run it
# names may have completed between the list and the call.
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
# JOB_TIMEOUT_MINUTES and fails by name past it: a refused list fails the step as it did, and a
# refused cancel warns as it did.
#
# Run from the repository root, with those three and $GITHUB_REPOSITORY set:
#   .github/scripts/cancel-started-act.sh
# CI runs it in ci.yaml's cancel-started job, at its second step, after the checkout.
set -euo pipefail
# shellcheck source=.github/scripts/gh-read.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/gh-read.sh"

# The `--limit` is 100, well above the number of groups main's merge queue builds at once, and
# this pull request's own groups are among those: gh's default of 20 runs could leave an older
# group of it out of the list, and so out of the act.
RUNS=$(gh_wait_reset gh run list --repo "$GITHUB_REPOSITORY" --workflow ci.yaml \
  --event merge_group --limit 100 --json databaseId,headBranch,status,createdAt)
export RUNS
acts=$(.github/scripts/merge-group-cancels.sh)
while read -r act id; do
  [ "$act" = cancel ] || continue
  gh_wait_reset gh api --silent --method POST \
    "repos/$GITHUB_REPOSITORY/actions/runs/$id/force-cancel" ||
    echo "::warning::force-cancel of run $id failed; it may have completed already"
done <<<"$acts"
