#!/usr/bin/env bash
# The selection that the affected job in ci.yaml runs (.github/scripts/go_legs_select.py says what
# it holds and why a change selects what it selects): writes the step output affected_packages to
# $GITHUB_OUTPUT, the legs of each Go legs job the change starts and the packages they run, from
# the commit the change is measured from:
#   * a pull request: the first parent of the merge commit the run checked out, so the diff is the
#     pull request's change. Not the event's pull_request.base.sha: that is the base branch's tip
#     when the event was sent, which the merge commit GitHub builds later is often not on, and a
#     diff from it counts main's own newer commits as the pull request's;
#   * a merge group, while ci.yaml's CI_MERGE_GROUP_SELECTS is "true": its base (GO_LEGS_BASE, the
#     event's merge_group.base_sha), main or the entry ahead of it in the queue, so the diff is the
#     change the entry adds, its pull request's. Main's queue builds one pull request per entry,
#     each on the one ahead, and merges an entry only when every entry ahead of it passed: a
#     package only an entry ahead reaches was run by that entry, on a tree whose inputs for it this
#     entry does not change (it would reach the package if it did), so no entry merges with a
#     package any change in it reaches untested. A diff from main instead would hold every entry
#     ahead, which in the backtest of .github/ci-notes/ci.md#which-packages-a-go-leg-runs reached
#     nearly every package. While CI_MERGE_GROUP_SELECTS is anything else, every package, as
#     before this switch existed;
#   * a workflow_dispatch of a branch: its merge base with main (GO_LEGS_BASE when the caller has
#     it, else .github/scripts/edited-migrations-base.sh's, which fetches both histories until it
#     finds it); of main, or with every-job (EVERY_JOB=true), every package.
# Any failure - no base, a fetch that fails, no merge base in the history fetched, the selection
# itself - writes every leg with every package instead and says why. When this step itself does
# not finish (it runs out of time, the runner is lost), the affected job fails, and ci.yaml's legs
# callers then hand their files no selection, which also runs every leg with every package.
#
# Run from anywhere: .github/scripts/go-legs-select.sh
# CI runs it in ci.yaml's affected job. Its tests are in go-legs-select.test.sh.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

out=${GITHUB_OUTPUT:-/dev/stdout}
every=$(python3 .github/scripts/go_legs_select.py --every) || every=

# fetch <depth> <what...>: `git fetch --depth` only where the checkout is already shallow (CI's
# one-commit checkout), and a whole fetch otherwise: a depth fetch in a whole clone writes its
# edge into the clone's one shallow file, which every worktree of that clone shares, and cuts
# their history.
fetch() {
  local depth=$1
  shift
  if [ "$(git rev-parse --is-shallow-repository)" = true ]; then
    git fetch -q --no-tags --depth="$depth" origin "$@" >&2
  else
    git fetch -q --no-tags origin "$@" >&2
  fi
}

# pick: the commit the change is measured from on stdout, or a failure with why on stderr.
pick() {
  local base=${GO_LEGS_BASE:-}
  case "${GITHUB_EVENT_NAME:-}" in
    pull_request)
      git rev-parse -q --verify "HEAD^1^{commit}" >/dev/null || fetch 2 "${GITHUB_SHA:?}" || return 1
      base=$(git rev-parse HEAD^1) || return 1
      echo "the change is the pull request's, from the merge commit's first parent $base" >&2 ;;
    merge_group)
      if [ "${CI_MERGE_GROUP_SELECTS:-}" != true ]; then
        echo "a merge group, and ci.yaml's CI_MERGE_GROUP_SELECTS is '${CI_MERGE_GROUP_SELECTS:-}', not true" >&2
        return 1
      fi
      [ -n "$base" ] || { echo "a merge group run with no base" >&2; return 1; }
      echo "the change is the merge group entry's own, from the entry ahead of it (or main) $base" >&2 ;;
    workflow_dispatch)
      if [ "${GITHUB_REF:-}" = refs/heads/main ] || [ "${EVERY_JOB:-}" = true ]; then
        echo "a dispatch of main, or one given every-job" >&2
        return 1
      fi
      if [ -z "$base" ]; then
        base=$(.github/scripts/edited-migrations-base.sh) || return 1
      fi
      echo "the change is the branch's, from its merge base with main $base" >&2 ;;
    *)
      echo "event ${GITHUB_EVENT_NAME:-<unset>} has no change this selection reads" >&2
      return 1 ;;
  esac
  if ! git cat-file -e "$base^{commit}" 2>/dev/null; then
    fetch 1 "$base" || return 1
  fi
  echo "$base"
}

started=$(date +%s)
if base=$(pick) && selected=$(python3 .github/scripts/go_legs_select.py --base "$base") && [ -n "$selected" ]; then
  echo "the selection took $(($(date +%s) - started)) s"
else
  echo "every leg runs every package: the lines above say why"
  selected=$every
fi
echo "affected_packages=$selected" >>"$out"
echo "affected_packages: $selected"
