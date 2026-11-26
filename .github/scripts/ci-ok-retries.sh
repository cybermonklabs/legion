#!/usr/bin/env bash
# .github/scripts/ci-ok-retries.sh
#
# Names, in ci-ok's job summary, what the Go steps' retry let through: each test that passed only
# on its retry, a flake the merge group did not stop on, and each package that failed with no
# test failing (a TestMain check after its tests passed, or a wrapper's own exit), which
# .github/scripts/go-test.sh fails its step on whatever a rerun did. It reads the files the Go
# steps upload and ci-ok downloads under one directory, each artifact in a directory named for it
# (the legs of one job write the same jsonfile names):
#
#   ci-ok-retries.sh <directory>
#
# Each <name>.json under the directory, at any depth, is a gotestsum --jsonfile, and the
# <name>.rerun-fails.txt beside it, where there is one, that run's --rerun-fails-report: one line
# per test that failed at least once, `<package>.<test>: <N> runs, <M> failures`. A test with more
# runs than failures passed on a rerun. A package that failed with no test failing is read off the
# jsonfile with .github/scripts/go-test.jq's no_test_failures, attempt by attempt. A pull
# request's run takes no retry and writes no rerun report, so it can name no flake.
#
# It writes a section to $GITHUB_STEP_SUMMARY, where that is set, and the same lines to stdout.
# A directory with no jsonfile, which is every run whose Go jobs were all skipped, is a line saying
# so. It names a file jq cannot read as one it could not read, with a warning, and goes on: what
# ci-ok decides is its verdict step's, and this report only names what it read. It exits 2 on a
# usage error and 0 otherwise.
set -euo pipefail
export LC_ALL=C

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

if [ $# -ne 1 ] || [ ! -d "$1" ]; then
  echo "ci-ok-retries.sh: give the directory the Go steps' jsonfiles were downloaded to (usage: ci-ok-retries.sh <directory>)" >&2
  exit 2
fi
directory=$1

mapfile -t jsonfiles < <(find "$directory" -type f -name '*.json' | sort)

flakes=()
lone=()
unread=()
reports=0
for jsonfile in "${jsonfiles[@]}"; do
  name=${jsonfile#"$directory"/}
  rerun_report="${jsonfile%.json}.rerun-fails.txt"
  if [ -f "$rerun_report" ]; then
    reports=$((reports + 1))
    while IFS= read -r line; do
      [[ "$line" =~ ^(.+):\ ([0-9]+)\ runs,\ ([0-9]+)\ failures$ ]] || continue
      if [ "${BASH_REMATCH[2]}" -gt "${BASH_REMATCH[3]}" ]; then
        flakes+=("\`${BASH_REMATCH[1]}\`: ${BASH_REMATCH[2]} runs, ${BASH_REMATCH[3]} failures, in $name")
      fi
    done <"$rerun_report"
  fi
  if ! found=$(jq -n -r -L "$here" 'include "go-test"; no_test_failures(inputs) | "\(.package)\t\(.attempt)"' "$jsonfile" 2>&1); then
    unread+=("$name: $(head -c 200 <<<"$found")")
    continue
  fi
  while IFS=$'\t' read -r package attempt; do
    [ -n "$package" ] || continue
    lone+=("\`$package\`, in attempt $attempt, in $name")
  done <<<"$found"
done

{
  echo "### Go retries"
  echo
  if [ "${#jsonfiles[@]}" -eq 0 ]; then
    echo "No Go step uploaded a jsonfile to this run, so there is no retry to name."
  else
    echo "Read ${#jsonfiles[@]} jsonfiles and $reports rerun reports."
    echo
    if [ "${#flakes[@]}" -eq 0 ]; then
      echo "No Go test passed only on its retry."
    else
      echo "Go tests that passed only on their retry:"
      echo
      printf -- '- %s\n' "${flakes[@]}"
    fi
    echo
    if [ "${#lone[@]}" -eq 0 ]; then
      echo "No Go package failed with no test failing."
    else
      echo "Go packages that failed with no test failing, which fails their step whatever a rerun did:"
      echo
      printf -- '- %s\n' "${lone[@]}"
    fi
    if [ "${#unread[@]}" -gt 0 ]; then
      echo
      echo "Files this report could not read, whose retries it cannot name:"
      echo
      printf -- '- %s\n' "${unread[@]}"
    fi
  fi
} | if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then tee -a "$GITHUB_STEP_SUMMARY"; else cat; fi

for entry in "${unread[@]}"; do
  echo "::warning::ci-ok-retries.sh: could not read $entry"
done
exit 0
