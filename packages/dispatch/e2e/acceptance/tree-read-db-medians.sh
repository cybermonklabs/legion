#!/usr/bin/env bash
# The median stage of check c-tree-read-bound of the issue tree's Verification document: it judges
# the database time of the tree read against the old issue page's, over the logs of the three build
# Jobs that ran the check's `job` stage, each read from the newest pod of its Job.
#
# Each Job's run of TestTheTreeReadAnswersTheSkeletonWithinItsBounds
# (packages/envoy/internal/dispatch/api/tree_read_skeleton_bound_test.go) prints one line for each
# fixture and each read of the tree route:
#
#   c-tree-read-bound: database time per reading: fixture=<label> read=<filter> old=<ms> new=<ms>
#
# where old is the old page's database time per reading of the reads that filter is held to and new
# the tree read's own. This script requires a named PASS of that test in each log, and in each log
# exactly one numeric pair for each of the four fixtures and the six filters it names below, the
# stable labels the test prints: a pair missing, a pair printed twice in one log (a Job whose pods
# ran twice, read whole), a pair whose value is no number, and a pair of a fixture or a filter it
# does not name each fail the stage, named. It prints, for each fixture and filter, the median of the
# three old times and the median of the three new times, with each Job's value, and holds the rule
# R8 and R30 state separately for each of them, never over a sum: the new median is at most twice
# the old median, or, where the new median is under 2 ms, at most 1 ms above the old median.
#
# Usage: bash tree-read-db-medians.sh <log of Job 1> <log of Job 2> <log of Job 3>
# It exits 0 once every pair holds the rule, and 1 otherwise, after printing every pair; 2 on bad
# usage.
set -euo pipefail
set +x

if [ "$#" -ne 3 ]; then
  echo "usage: bash tree-read-db-medians.sh <log of Job 1> <log of Job 2> <log of Job 3>" >&2
  exit 2
fi
for log in "$@"; do
  if [ ! -r "$log" ]; then
    echo "tree-read-db-medians.sh: no readable log $log" >&2
    exit 2
  fi
done

# The fixtures and the filters of the test's lines (skeletonShapes' labels and skeletonFilters'
# names), each line one pair.
fixtures="largest-plan most-comments most-blocks largest-spec"
filters="no-view kind spec verification plan decisions"

awk -v fixtures="$fixtures" -v filters="$filters" '
function numeric(value) {
  return value ~ /^[0-9]+(\.[0-9]+)?$/
}
function median(a, b, c,    swap) {
  if (a > b) { swap = a; a = b; b = swap }
  if (b > c) { swap = b; b = c; c = swap }
  if (a > b) { swap = a; a = b; b = swap }
  return b
}
FNR == 1 {
  job++
  name[job] = FILENAME
}
{
  line = $0
  gsub(/\r/, "", line)
}
line ~ /--- PASS: TestTheTreeReadAnswersTheSkeletonWithinItsBounds / {
  passed[job] = 1
}
{
  at = index(line, "c-tree-read-bound: database time per reading: ")
  if (at == 0) {
    next
  }
  fields = split(substr(line, at), field, " ")
  if (fields != 9 || field[6] !~ /^fixture=/ || field[7] !~ /^read=/ || field[8] !~ /^old=/ || field[9] !~ /^new=/) {
    printf "c-tree-read-bound: %s holds a line of database time of another shape: %s\n", name[job], line
    bad = 1
    next
  }
  pair = substr(field[6], 9) " " substr(field[7], 6)
  count[job, pair]++
  old[job, pair] = substr(field[8], 5)
  new[job, pair] = substr(field[9], 5)
  seen[pair] = 1
}
END {
  for (job = 1; job <= 3; job++) {
    if (!passed[job]) {
      printf "c-tree-read-bound: %s holds no line --- PASS: TestTheTreeReadAnswersTheSkeletonWithinItsBounds\n", name[job]
      bad = 1
    }
  }
  named = split(fixtures, fixture, " ")
  read = split(filters, filter, " ")
  for (i = 1; i <= named; i++) {
    for (k = 1; k <= read; k++) {
      expected[fixture[i] " " filter[k]] = 1
    }
  }
  for (pair in seen) {
    if (!(pair in expected)) {
      split(pair, part, " ")
      printf "c-tree-read-bound: fixture=%s read=%s is a pair of no fixture and filter the check names\n", part[1], part[2]
      bad = 1
    }
  }
  for (i = 1; i <= named; i++) {
    for (k = 1; k <= read; k++) {
      pair = fixture[i] " " filter[k]
      measured = 1
      for (job = 1; job <= 3; job++) {
        if (count[job, pair] == 0) {
          printf "c-tree-read-bound: fixture=%s read=%s: %s holds no old/new pair\n", fixture[i], filter[k], name[job]
          measured = 0
        } else if (count[job, pair] > 1) {
          printf "c-tree-read-bound: fixture=%s read=%s: %s holds %d pairs in one log, want one\n", fixture[i], filter[k], name[job], count[job, pair]
          measured = 0
        } else if (!numeric(old[job, pair]) || !numeric(new[job, pair])) {
          printf "c-tree-read-bound: fixture=%s read=%s: %s holds an unmeasured pair, old=%s new=%s\n", fixture[i], filter[k], name[job], old[job, pair], new[job, pair]
          measured = 0
        }
      }
      if (!measured) {
        bad = 1
        continue
      }
      before = median(old[1, pair] + 0, old[2, pair] + 0, old[3, pair] + 0)
      now = median(new[1, pair] + 0, new[2, pair] + 0, new[3, pair] + 0)
      within = now <= 2 * before || (now < 2 && now <= before + 1)
      verdict = "within the bound"
      if (!within) {
        verdict = "over the bound: the new median is above twice the old median, and not under 2 ms within 1 ms of it"
        bad = 1
      }
      printf "c-tree-read-bound: fixture=%s read=%s: old median %.3f ms (%s, %s, %s), new median %.3f ms (%s, %s, %s): %s\n", \
        fixture[i], filter[k], before, old[1, pair], old[2, pair], old[3, pair], now, new[1, pair], new[2, pair], new[3, pair], verdict
    }
  }
  if (bad) {
    print "FAIL: tree-read-db-medians.sh: a pair is missing, duplicated, unmeasured or over the bound, each printed above"
    exit 1
  }
  printf "PASS: tree-read-db-medians.sh complete: %d fixtures and %d filters, each new median within the bound of its old median\n", named, read
}
' "$@"
