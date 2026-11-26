#!/usr/bin/env bash
# edited-migrations-base picks the base the edited-migrations check compares a run's tree with
# (.github/workflows/ci-job-envoy-go-build.yaml, the step "Check that no migration a database
# may have run is edited without its replaces line"), fetches it into the checkout, and prints
# its sha. actions/checkout leaves the tree one commit deep, and the check reads the base's files
# with git, so the base has to be fetched before the check runs.
# Every fetch goes through fetch-depth-if-shallow.sh: in CI's shallow checkout it fetches with
# the depth written here, which deepens the checkout only as far as the base needs; in a whole
# clone, such as a developer's, it fetches with no depth, which would make the clone shallow and
# cut the history that every worktree of the clone reads.
# ci.yaml's changes job runs it too, on a dispatch of a branch that selects by its diff, and hands
# the merge base it prints to the filter generation as CI_FILTERS_BASE
# (.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job).
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
#     a run of a branch started by hand (`gh workflow run ci.yaml --ref <branch>`).
#     The script fetches the two histories deeper until git finds a commit both hold and every
#     commit above it is fetched with its parents, so no newer one can hide past the fetch, and
#     exits 2 when the whole of both shares none, or when git merge-base fails rather than answers.
#
# Its inputs, from the step's env and the runner's:
#   GITHUB_EVENT_NAME  the event, as Actions sets it.
#   GITHUB_SHA         the commit the run checked out, as Actions sets it.
#   GITHUB_REF         workflow_dispatch only: the ref the run was started on.
#   MIGRATION_BASE     workflow_dispatch only: ci.yaml's migration-base input; may be empty.
#
# Stdout holds the base's sha and nothing else. Every message goes to stderr: the line that names
# the base and why, a refusal as an ::error:: line, and git's own output. Any other command that
# fails, a fetch among them, exits with that command's status, so the check fails closed.
#
# Run inside the checkout: .github/scripts/edited-migrations-base.sh
# Its tests are in edited-migrations-base.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

event=${GITHUB_EVENT_NAME:?GITHUB_EVENT_NAME must name the event}
sha=${GITHUB_SHA:?GITHUB_SHA must name the commit the run checked out}

if [ "$event" != workflow_dispatch ]; then
  "$script_dir/fetch-depth-if-shallow.sh" --depth=2 --no-tags origin "$sha" >&2
  base=$(git rev-parse HEAD^1)
  echo "edited-migrations base: $base, the first parent of $sha, the commit this $event run checked out" >&2
  echo "$base"
  exit 0
fi

ref=${GITHUB_REF:?GITHUB_REF must name the ref the run was started on}
if [ -n "${MIGRATION_BASE:-}" ]; then
  "$script_dir/fetch-depth-if-shallow.sh" --depth=1 --no-tags origin "$MIGRATION_BASE" >&2
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
# latest. In a whole clone the first fetch takes no depth and the repository stays whole, so the
# loop ends at its first pass. The passes after the first name main's tip by its sha: git sends
# no want for a refspec whose destination already holds the tip it would fetch, and main's history
# would then stay 64 commits deep.
#
# The base git merge-base finds in the fetched commits is sure to be the merge base of the whole
# histories when no commit above it was cut off. A branch can hold a newer commit of main only
# along a path longer than the fetch (a merge of main far down the branch) beside a short path to
# an older one (a merge, near its tip, of a branch built on an older commit of main): the fetch
# then joins the branch to the older commit of main and not to the newer, and git answers the
# older. So the loop takes a base only when no commit that HEAD or main holds and the base does
# not is a shallow commit, one the fetch left without its parents (git lists them in the shallow
# file). Then every commit above the base is fetched with all its parents, so a common commit above
# the base would be among them, and git would answer it instead. git rev-list counts the commits
# above the base twice, the second time with every shallow commit after --not: git reads no
# parents of a shallow commit, so that drops the shallow commit alone, and the two counts match
# only when none of them is above the base.
# The fetched commits cannot show that a commit cut off below a branch's fork point is under the
# base too, so a branch that merged main far above its fork point is fetched deeper as well: that
# costs time, not the answer.
# git merge-base exits 1 when the two commits share no ancestor in what is fetched, and the loop
# fetches deeper then too; any other code is a fault (git exits 128 on a missing or unreadable
# object), not an answer, so the script prints git's message and exits 2 naming the code.
main=refs/remotes/origin/main
depth=64
"$script_dir/fetch-depth-if-shallow.sh" --depth="$depth" --no-tags origin "$sha" "+refs/heads/main:$main" >&2
main_sha=$(git rev-parse "$main")
shallow_file=$(git rev-parse --git-path shallow)
while :; do
  if base=$(git merge-base HEAD "$main_sha"); then
    found=0
  else
    found=$?
  fi
  if [ "$found" -gt 1 ]; then
    echo "::error::git merge-base HEAD $main_sha exited $found, not 0 (a base) or 1 (no base), so this workflow_dispatch run on $ref could not tell its base; git's message is above" >&2
    exit 2
  fi
  shallow_repo=$(git rev-parse --is-shallow-repository)
  if [ "$shallow_repo" = false ]; then
    if [ "$found" -eq 1 ]; then
      echo "::error::HEAD $sha and main $main_sha share no commit, so this workflow_dispatch run on $ref has no base to compare its migrations with; name one with -f migration-base=<commit>" >&2
      exit 2
    fi
    break
  fi
  if [ "$found" -eq 0 ]; then
    mapfile -t cut_off < "$shallow_file"
    above=$(git rev-list --count HEAD "$main_sha" --not "$base")
    above_uncut=$(git rev-list --count HEAD "$main_sha" --not "$base" "${cut_off[@]}")
    if [ "$above" -eq "$above_uncut" ]; then
      break
    fi
  fi
  depth=$((depth * 4))
  "$script_dir/fetch-depth-if-shallow.sh" --depth="$depth" --no-tags origin "$sha" "$main_sha" >&2
done
echo "edited-migrations base: $base, the merge base of HEAD $sha with main $main_sha: this workflow_dispatch run on $ref names no migration-base, and a branch lands onto main" >&2
echo "$base"
