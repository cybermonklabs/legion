#!/usr/bin/env bash
# cache-superseded.sh <mode> [<family>]: deletes the Actions cache entries a fill of
# .github/workflows/ci-caches.yaml superseded, keeping the one it names. Its *-superseded jobs run
# it, each in a job of its own so the token that may delete caches is in no job that builds code
# (.github/ci-notes/ci-caches.md#what-the-refill-superseded-job-deletes-and-why-it-is-a-job-of-its-own).
#
#   commit <family>  refill-superseded: KEEP is the key a fill saved, the readers' key followed by
#                    `-$SHA`. Deletes every other entry of REF whose key is the readers' key or
#                    starts with it and a `-`.
#   hash <family>    envoy-infra-superseded: KEEP is a key holding a 64-hex hash of module files.
#                    Deletes every other entry of REF of the family whose key is KEEP but for that
#                    hash: the same runner and Go, older module files, the hashless key included.
#   build            dispatch-builds-superseded: KEEP is a Dispatch build's key, the build, the
#                    runner's OS and architecture, the script's 64-hex hash and the 40-hex commit.
#                    Deletes REF's entries of that build, OS and architecture created before
#                    KEEP's, and every one of any other ref.
#
# Its environment: KEEP, REF (the ref whose entries it reads, github.ref), SHA (commit mode,
# github.sha), and GH_TOKEN and GH_REPO for gh. None is deleted unless KEEP is among REF's entries,
# so a save that did not land leaves the old entries in place. A cache key is text any build that
# saves on a ref chose, so no key is split or printed raw: the list stays JSON, jq selects the
# numeric ids to delete, and each id is checked to be a number before `gh cache delete` gets it, so
# a key holding a newline cannot add a line of its own, such as `--all`, to that list. Each gh call
# waits out a spent API budget within the job's limit (JOB_TIMEOUT_MINUTES) and fails by name past
# it (gh-read.sh).
set -euo pipefail
# shellcheck source=.github/scripts/gh-read.sh
source "$(dirname "$0")/gh-read.sh"

mode=${1:?usage: cache-superseded.sh commit|hash <family>, or cache-superseded.sh build}
family=${2:-}
: "${KEEP:?KEEP must name the key the fill saved}" "${REF:?REF must name the ref whose entries are read}"

# delete <what> <why>: deletes each id read on stdin, refusing one that is not a number.
delete() {
  while read -r id; do
    if ! [[ $id =~ ^[0-9]+$ ]]; then
      echo "::error::the cache list gave an id that is not a number, so nothing more is deleted"
      exit 1
    fi
    echo "Deleting $1 entry $id, $2"
    gh_wait_reset gh cache delete "$id"
  done
}

# kept <entries> <what>: whether KEEP is among REF's entries, warning when it is not.
kept() {
  jq -e --arg keep "$KEEP" --arg ref "$REF" 'any(.[]; .key == $keep and .ref == $ref)' <<<"$1" >/dev/null && return
  echo "::warning::the new $2 entry is not among this ref's entries, so none is deleted"
  return 1
}

case "$mode:$family" in
  commit:?*)
    # The readers' key: the saved one without the commit the fill put at its end.
    base=${KEEP%"-${SHA:?SHA must name the commit the fill saved under}"}
    if [ "$base-$SHA" != "$KEEP" ]; then
      echo "::error::the $family key this leg saved does not end in this run's commit, so nothing is deleted"
      exit 1
    fi
    # Newest first, by GitHub's own creation order, so the new entry is among the entries read
    # however many older ones a failed run left behind.
    entries=$(gh_wait_reset gh cache list --ref "$REF" --key "$base" --sort created_at --order desc --limit 100 --json id,key,ref)
    kept "$entries" "$family" || exit 0
    jq -r --arg keep "$KEEP" --arg base "$base" --arg ref "$REF" \
      '.[] | select(.ref == $ref and .key != $keep and (.key == $base or (.key | startswith($base + "-")))) | .id' <<<"$entries" |
      delete "$family" "which the new entry supersedes"
    ;;
  hash:?*)
    entries=$(gh_wait_reset gh cache list --ref "$REF" --key "$family-" --limit 100 --json id,key,ref)
    kept "$entries" "$family" || exit 0
    jq -r --arg keep "$KEEP" --arg ref "$REF" --arg family "$family-" \
      'def base: gsub("-[0-9a-f]{64}"; "");
       .[] | select(.ref == $ref and (.key | startswith($family)) and .key != $keep
                    and (.key | base) == ($keep | base)) | .id' <<<"$entries" |
      delete "$family" "which the new entry supersedes"
    ;;
  build:)
    if ! [[ $KEEP =~ ^dispatch-(previous|first-rollout)-build-[A-Za-z0-9]+-[A-Za-z0-9]+-[0-9a-f]{64}-[0-9a-f]{40}$ ]]; then
      echo "::error::the kept key does not have the shape dispatch-builds gives it, so nothing is deleted"
      exit 1
    fi
    # The build, OS and architecture: the key without its script hash and commit.
    prefix=${KEEP%-*}
    prefix=${prefix%-*}-
    # Every ref's entries, newest first by GitHub's creation order; gh pages past 100 up to the
    # limit, far above what one build holds.
    entries=$(gh_wait_reset gh cache list --key "$prefix" --sort created_at --order desc --limit 1000 --json id,key,ref)
    if ! jq -e --arg keep "$KEEP" --arg ref "$REF" 'any(.[]; .key == $keep and .ref == $ref)' <<<"$entries" >/dev/null; then
      echo "::warning::main holds no entry under $KEEP, so none is deleted"
      exit 0
    fi
    jq -r --arg keep "$KEEP" --arg ref "$REF" --arg prefix "$prefix" \
      '(map(.key == $keep and .ref == $ref) | index(true)) as $kept
       | to_entries[] | .key as $at | .value
       | select((.key | startswith($prefix)) and (.ref != $ref or $at > $kept)) | .id' <<<"$entries" |
      delete "$prefix" "which this repository keeps no more"
    ;;
  *)
    echo "::error::usage: cache-superseded.sh commit|hash <family>, or cache-superseded.sh build" >&2
    exit 2
    ;;
esac
