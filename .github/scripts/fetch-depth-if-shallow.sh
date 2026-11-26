#!/usr/bin/env bash
# fetch-depth-if-shallow.sh <depth option> <git fetch argument>...: runs
# `git fetch <depth option> <argument>...` in a shallow repository, and `git fetch <argument>...`
# in a whole one. The depth option is one of --depth=<n>, --deepen=<n>, --shallow-since=<date> or
# --shallow-exclude=<ref>.
#
# actions/checkout leaves a run's tree one commit deep, so a script that reads a commit below it
# fetches that commit with a depth, which deepens the checkout only as far as the script needs.
# The same fetch in a whole clone, such as a developer's, makes the clone shallow: git records the
# commits at the depth's edge in its shallow file, and every later read treats them as having no
# parents, although their parents are in the clone. A clone that many worktrees share has one
# shallow file, so a fetch in any one of them cuts the history all of them read. git merge-base,
# git merge-tree and an ancestor test then do not fail; they answer from the commits still in
# view, so a conflict check can read clean. A whole clone holds every commit below one it fetches
# already, or gets them from this fetch, so it needs no depth.
#
# The shallow test is git's own (`git rev-parse --is-shallow-repository`), read in the repository
# the fetch writes to. Anything but `true` or `false` from it, or a git that fails to answer,
# stops the script before any fetch, with git's status or 2.
#
# Run inside the repository: .github/scripts/fetch-depth-if-shallow.sh --depth=2 origin <sha>
# .github/scripts/edited-migrations-base.sh and .github/scripts/ci-filters-generate.sh fetch
# through it. Its tests are in fetch-depth-if-shallow.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

case "${1:-}" in
  --depth=?* | --deepen=?* | --shallow-since=?* | --shallow-exclude=?*) ;;
  *)
    echo "::error file=.github/scripts/fetch-depth-if-shallow.sh::the first argument must be --depth=<n>, --deepen=<n>, --shallow-since=<date> or --shallow-exclude=<ref>, not '${1:-}'" >&2
    exit 2
    ;;
esac
option=$1
shift

shallow=$(git rev-parse --is-shallow-repository)
case "$shallow" in
  true) exec git fetch "$option" "$@" ;;
  false) exec git fetch "$@" ;;
  *)
    echo "::error file=.github/scripts/fetch-depth-if-shallow.sh::git rev-parse --is-shallow-repository printed '$shallow', not true or false, so whether a depth would make this repository shallow is unknown" >&2
    exit 2
    ;;
esac
