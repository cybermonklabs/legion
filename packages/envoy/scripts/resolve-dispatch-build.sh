#!/usr/bin/env bash
# Prints the full sha of the Dispatch build build-dispatch-build.sh would build, after the same
# refusals, and builds nothing: a caller keys a cache of the build's directory by it before the
# build (CI's dispatch-two-builds job), and build-dispatch-build.sh runs it to find what it builds.
#
#   packages/envoy/scripts/resolve-dispatch-build.sh --commit <sha|tag>
#   packages/envoy/scripts/resolve-dispatch-build.sh --previous-release
#
# --previous-release resolves the newest legion-envoy-v* tag that is an ancestor of
# `git merge-base HEAD origin/main`: the release production ran before this branch. --commit HEAD
# resolves the current branch.
#
# The refusals: a commit that is not an ancestor of HEAD, and a commit older than the release
# legion-envoy-v30.11.0, the oldest build either two-build test can run as the earlier side
# (build-dispatch-build.sh's header says why).
#
# It is a script of its own, not a mode of build-dispatch-build.sh, so a step that resolves runs no
# script that installs anything: .github/scripts/check-ci-filters.sh's rule 22 reads every command
# of a script a step runs, whatever its arguments, and counts the build script's bounded `bun
# install` against each step that runs it.
set -euo pipefail

usage() {
  printf 'usage: %s (--commit <sha|tag> | --previous-release)\n' "$0" >&2
  exit 2
}

fail() {
  printf 'resolve-dispatch-build: %s\n' "$*" >&2
  exit 1
}

note() {
  printf 'resolve-dispatch-build: %s\n' "$*" >&2
}

commit=
previous_release=
while (($# > 0)); do
  case "$1" in
  --commit)
    (($# >= 2)) || usage
    commit=$2
    shift 2
    ;;
  --previous-release)
    previous_release=1
    shift
    ;;
  *) usage ;;
  esac
done
[[ -n "$commit" && -z "$previous_release" || -z "$commit" && -n "$previous_release" ]] || usage
command -v git >/dev/null || fail "the command git is necessary. Install it and run this script again."

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)
readonly root
readonly release_floor=legion-envoy-v30.11.0

if [[ -n "$previous_release" ]]; then
  git -C "$root" rev-parse --verify --quiet origin/main >/dev/null ||
    fail "this repository has no origin/main. Fetch origin/main and its tags, then run this script again."
  base=$(git -C "$root" merge-base HEAD origin/main) || fail "HEAD and origin/main have no merge base."
  mapfile -t tags < <(git -C "$root" tag --list 'legion-envoy-v*' --sort=-v:refname --merged "$base")
  ((${#tags[@]} > 0)) ||
    fail "no legion-envoy-v* tag is an ancestor of the merge base $base of HEAD and origin/main. Fetch the tags, then run this script again."
  commit=${tags[0]}
  note "the previous release is $commit (the newest legion-envoy-v* tag in the history of $base)"
fi

sha=$(git -C "$root" rev-parse --verify --quiet "$commit^{commit}") ||
  fail "$commit is not a commit of this repository."
git -C "$root" merge-base --is-ancestor "$sha" HEAD ||
  fail "$commit ($sha) is not an ancestor of HEAD. Give a commit in the history of HEAD."

floor_sha=$(git -C "$root" rev-parse --verify --quiet "$release_floor^{commit}") ||
  fail "this repository has no tag $release_floor, the oldest release a build of this script may be. Fetch the tags, then run this script again."
git -C "$root" merge-base --is-ancestor "$floor_sha" "$sha" ||
  fail "$commit ($sha) is older than the release $release_floor. The two-build tests need that release or later as the earlier build: it shares one database with this tree's build and writes the default assignee every project now requires (migration 20261111210103), serves on the socket the browser harness holds for it, and signs cookies with that run's shared dev sign-in key."

printf '%s\n' "$sha"
