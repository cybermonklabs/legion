#!/usr/bin/env bash
# fetch-depth-if-shallow.sh --depth=<n> <git fetch argument>...: runs
# `git fetch --depth=<n> <argument>...` in a shallow repository, and `git fetch <argument>...` in
# a whole one. The first argument must be --depth=<n>: no caller passes another depth option.
#
# actions/checkout leaves a run's tree one commit deep, so a script that reads a commit below it
# fetches that commit with a depth, which deepens the checkout only as far as the script needs.
# The same fetch in a whole clone, such as a developer's, makes the clone shallow: git records the
# commits at the depth's edge in its shallow file, and every later read treats them as having no
# parents, although their parents are in the clone. A clone that many worktrees share has one
# shallow file, so a fetch in any one of them cuts the history all of them read. git merge-base
# and an ancestor test then answer from the commits still in view: a commit below the edge reads
# as no ancestor, and a merge base reads as none or as an older commit, so a check that trusts
# them can read clean. A whole clone holds every commit below one it fetches already, or gets them
# from this fetch, so it needs no depth.
#
# The shallow test is git's own (`git rev-parse --is-shallow-repository`), read in the repository
# the fetch writes to. Anything but `true` or `false` from it, or a git that fails to answer,
# stops the script before any fetch, with git's status or 2.
#
# Run inside the repository: .github/scripts/fetch-depth-if-shallow.sh --depth=2 origin <sha>
# .github/scripts/edited-migrations-base.sh and .github/scripts/ci-filters-generate.sh fetch
# through it. check-ci-filters.sh's rule 26 refuses a git fetch or git pull that names a depth
# option under .github/scripts, .github/actions and .github/workflows, but a fixture fetch
# ci_job_reads.py lists; this script's own fetch passes the depth in a variable, which the rule
# does not read. Its tests are in fetch-depth-if-shallow.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

case "${1:-}" in
  --depth=?*) ;;
  *)
    echo "::error file=.github/scripts/fetch-depth-if-shallow.sh::the first argument must be --depth=<n>, not '${1:-}'" >&2
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
