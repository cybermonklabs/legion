#!/usr/bin/env bash
# Resolves the base of a run to one commit id. Fetches that commit with its whole tree into a
# checkout of one commit. Writes the id to $GITHUB_OUTPUT as `sha`. Each form that compares the
# head with main's plugins or listener gets that id, and fetches nothing while it runs. Two legs of
# ci.yaml run this script:
#   - agent-harness's session-scripts leg, before envoy-session-delivery.sh's --plugins-from and
#     --resume-plugins-from steps (.github/actions/agent-harness).
#   - envoy-held's envoy-session-proofs leg, before smoke-role-takeover.sh's --base-rev step
#     (.github/actions/go-envoy/envoy-session-proofs).
#
# The base comes from the run, never from a name read later. The step gives it as BASE:
#   - a merge group: github.event.merge_group.base_sha, the commit the queue built the group on.
#   - a pull request: github.event.pull_request.base.sha, the head of its base branch.
# A workflow_dispatch run has neither, so BASE is empty. Then the base is main's head when this run
# attempt started. The script reads the run's run_started_at, and then the newest push to
# refs/heads/main at or before that time in the repository's activity. Both reads go through
# gh-read.sh's gh_read with GH_TOKEN. The activity read holds main's last 100 pushes (30 hours of
# them on 2026-11-25). When the start is older than all of them, the script fails by name.
#
# The fetch takes the commit with its tree (--depth=1, no blob filter): envoy-session-delivery.sh
# and smoke-role-takeover.sh read the tree with `git archive`. It writes the commit to
# refs/remotes/origin/compared-base: smoke-role-takeover.sh resolves the commit with jj, and jj
# imports only commits that a ref holds. The fetch gets three attempts of 60 s each, the pattern of
# fetch-release-history.sh (which gives 30 s to a fetch with no blobs). Between two attempts it ends
# every git process on the runner and deletes the locks that a killed git can leave. Run it from the
# checkout's root. The repository is public, so the fetch needs no token.
set -euo pipefail

what="the event's base"
if [ -z "${BASE:-}" ]; then
  # shellcheck source=.github/scripts/gh-read.sh
  source .github/scripts/gh-read.sh
  started=$(gh_read gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID" --jq .run_started_at)
  [[ $started =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] ||
    { echo "::error::run $GITHUB_RUN_ID's run_started_at is no UTC time: '$started'"; exit 1; }
  BASE=$(gh_read gh api "repos/$GITHUB_REPOSITORY/activity?ref=refs/heads/main&per_page=100" \
    --jq "[.[] | select(.timestamp <= \"$started\")][0].after // empty")
  [ -n "$BASE" ] ||
    { echo "::error::none of main's last 100 pushes is at or before this run's start, $started"; exit 1; }
  what="main's head when this run attempt started ($started)"
fi
[[ $BASE =~ ^[0-9a-f]{40}$ ]] || { echo "::error::the base '$BASE' is no commit id"; exit 1; }

for attempt in 1 2 3; do
  if timeout -k 5 60 git fetch --no-tags --depth=1 origin "+$BASE:refs/remotes/origin/compared-base"; then
    git rev-parse --verify --quiet "$BASE^{commit}" > /dev/null ||
      { echo "::error::the fetch of $BASE left no commit $BASE"; exit 1; }
    echo "the base, $what: $(git log -1 --format='%H %cI %s' "$BASE")"
    echo "sha=$BASE" >> "$GITHUB_OUTPUT"
    exit 0
  fi
  echo "attempt $attempt did not complete within 60 s"
  pkill -KILL -x git || true
  pkill -KILL -f git-remote-http || true
  while pgrep -x git > /dev/null || pgrep -f git-remote-http > /dev/null; do sleep 1; done
  find .git -name '*.lock' -delete
done
echo "::error::Three fetches of the base $BASE did not complete within 60 s each."
exit 1
