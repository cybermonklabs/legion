#!/usr/bin/env bash
# edited-migrations-base picks the base the edited-migrations check compares a run's tree with
# (.github/workflows/ci-job-envoy-go-checks.yaml, the step "Check that no migration a database
# may have run is edited without its replaces line"), fetches it into the checkout, and prints
# its sha. actions/checkout leaves the tree one commit deep, and the check reads the base's files
# with git, so the base has to be fetched before the check runs.
#
# The base is the commit the run's tree lands onto:
#   * any event but workflow_dispatch (a pull request, a merge group, a push): the first parent
#     of the commit the run checked out. That is the base of a pull request's merge commit, and
#     of a merge-group entry's commit (main, or the entry before it, whose own run checks its
#     change).
#   * a workflow_dispatch that names migration-base: that commit. The bypass check
#     (main-bypass-check.yaml) dispatches ci.yaml at main's tip after a push the queue never
#     tested, which can carry several commits, and passes the commit main pointed at before
#     that push.
#   * a workflow_dispatch on main that names none: no base. The tip's first parent would judge
#     the push's last commit alone, so the script exits 2, a check that could not be taken.
#   * a workflow_dispatch on any other ref that names none: the merge base of HEAD with main,
#     the newest commit of main the branch holds, which is what the branch lands onto. This is
#     how a full run of a branch is started by hand (`gh workflow run ci.yaml --ref <branch>`).
#     The script fetches the two histories deeper until git finds that commit, and exits 2 when
#     the whole of both shares none.
#
# Its inputs, from the step's env and the runner's:
#   GITHUB_EVENT_NAME  the event, as Actions sets it.
#   GITHUB_SHA         the commit the run checked out, as Actions sets it.
#   GITHUB_REF         workflow_dispatch only: the ref the run was started on.
#   MIGRATION_BASE     workflow_dispatch only: ci.yaml's migration-base input; may be empty.
#
# Stdout holds the base's sha and nothing else. Every message goes to stderr: the line that names
# the base and why, a refusal as an ::error:: line, and git's own output. A fetch that fails
# exits with git's status, so the check fails closed.
#
# Run inside the checkout: .github/scripts/edited-migrations-base.sh
# Its tests are in edited-migrations-base.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

event=${GITHUB_EVENT_NAME:?GITHUB_EVENT_NAME must name the event}
sha=${GITHUB_SHA:?GITHUB_SHA must name the commit the run checked out}

if [ "$event" != workflow_dispatch ]; then
  git fetch --no-tags --depth=2 origin "$sha" >&2
  base=$(git rev-parse HEAD^1)
  echo "edited-migrations base: $base, the first parent of $sha, the commit this $event run checked out" >&2
  echo "$base"
  exit 0
fi

ref=${GITHUB_REF:?GITHUB_REF must name the ref the run was started on}
if [ -n "${MIGRATION_BASE:-}" ]; then
  git fetch --no-tags --depth=1 origin "$MIGRATION_BASE" >&2
  echo "edited-migrations base: $MIGRATION_BASE, the migration-base this workflow_dispatch run on $ref names" >&2
  echo "$MIGRATION_BASE"
  exit 0
fi
if [ "$ref" = refs/heads/main ]; then
  echo "::error::this workflow_dispatch run names no migration-base, so the check cannot tell which commits it lands; main-bypass-check.yaml passes the commit main pointed at before the push" >&2
  exit 2
fi

# The first pass fetches 64 commits of each history and each pass after it four times as many,
# and a pass that reaches both roots leaves the repository whole, so the loop ends there at the
# latest. Main lands every change as one commit, so its history is a line, and a commit of main
# both fetched histories hold is one the branch lands onto. It is the newest such commit, the true
# merge base, unless the branch reaches a newer commit of main only along a path longer than the
# fetch (a merge of main far down the branch, beside a merge of an older main near its tip); then
# it is that older commit of main, which the branch holds too. The passes after the first name
# main's tip by its sha: git sends no want for a refspec whose destination already holds the tip
# it would fetch, and main's history would then stay 64 commits deep.
main=refs/remotes/origin/main
depth=64
git fetch --no-tags --depth="$depth" origin "$sha" "+refs/heads/main:$main" >&2
main_sha=$(git rev-parse "$main")
until base=$(git merge-base HEAD "$main_sha"); do
  if [ "$(git rev-parse --is-shallow-repository)" = false ]; then
    echo "::error::HEAD $sha and main $main_sha share no commit, so this workflow_dispatch run on $ref has no base to compare its migrations with; name one with -f migration-base=<commit>" >&2
    exit 2
  fi
  depth=$((depth * 4))
  git fetch --no-tags --depth="$depth" origin "$sha" "$main_sha" >&2
done
echo "edited-migrations base: $base, the merge base of HEAD $sha with main $main_sha: this workflow_dispatch run on $ref names no migration-base, and a branch lands onto main" >&2
echo "$base"
