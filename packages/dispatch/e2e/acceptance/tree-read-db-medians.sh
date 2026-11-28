#!/usr/bin/env bash
# The median stage of check c-tree-read-bound of the issue tree's Verification document: it judges
# the database time of the tree read against its recorded medians, over the logs of the three build
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
# three old times and the median of the three new times, with each Job's value, and the recorded
# median, and holds the rule of R8 and R30 separately for each of them, never over a sum: the new
# median is at most twice the recorded median, or, where the recorded median is under 2 ms, at most
# 1 ms above it. The old median is printed beside them and judged no more: the owner accepted the
# recorded medians in place of the old page's reads
# (dispatch://INFRA-1098/ask/4fa03306-8f00-4c13-af03-91e0aa995e98).
#
# The recorded medians are the 24 new medians of the Jobs infra-1253-tree-bound-dq7wv,
# infra-1253-tree-bound-9klm5 and infra-1253-tree-bound-spbcz at head 75c96d1b22
# (dispatch://INFRA-1253/comment/f3e1abb1-d4c7-4431-8c76-28ce45890f2e), each the median of its
# three new times as the Jobs printed them. They are in
# packages/envoy/internal/dispatch/api/testdata/tree-read-bound-recorded.json, which the script
# finds by its own place in the repository, from any directory, and reads with jq. That file
# missing or unreadable, or naming another head or other Jobs, fails the stage before any log is
# judged; a fixture and filter it records no number for, and a pair it records that the check does
# not name, each fail it, named, beside every pair it judges.
#
# Usage: bash tree-read-db-medians.sh <log of Job 1> <log of Job 2> <log of Job 3>
# It exits 0 once every pair holds the rule, and 1 otherwise, after printing every pair, or at once
# where the recorded file fails as above; 2 on bad usage or where jq is not installed.
set -euo pipefail
set +x

if [ "$#" -ne 3 ]; then
  echo "usage: bash tree-read-db-medians.sh <log of Job 1> <log of Job 2> <log of Job 3>" >&2
  exit 2
fi
for log in "$@"; do
  if [ ! -r "$log" ] || [ ! -s "$log" ]; then
    echo "tree-read-db-medians.sh: no readable, non-empty log $log" >&2
    exit 2
  fi
done
if ! command -v jq >/dev/null 2>&1; then
  echo "tree-read-db-medians.sh: jq is not installed; it reads the recorded medians" >&2
  exit 2
fi

# The recorded medians: their file, by this script's place in the repository, and the head and
# the Jobs the check names.
packages="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly packages
readonly recorded="$packages/envoy/internal/dispatch/api/testdata/tree-read-bound-recorded.json"
readonly recorded_head=75c96d1b22fdc2baba6195d60431ff4bac44b2bd
readonly recorded_jobs='["infra-1253-tree-bound-dq7wv","infra-1253-tree-bound-9klm5","infra-1253-tree-bound-spbcz"]'

# recorded_fault WHY: the recorded file is not the one the check names, so no pair is judged.
recorded_fault() {
  printf 'c-tree-read-bound: the recorded medians %s: %s\n' "$recorded" "$1"
  echo "FAIL: tree-read-db-medians.sh: no recorded median of head $recorded_head can be read, printed above"
  exit 1
}

if [ ! -f "$recorded" ] || [ ! -r "$recorded" ] || [ ! -s "$recorded" ]; then
  recorded_fault "no readable, non-empty file"
fi
fault="$(jq -rs --arg head "$recorded_head" --argjson jobs "$recorded_jobs" '
  if length != 1 then "it holds \(length) JSON values, not one"
  else .[0] |
    if type != "object" then "it holds no JSON object"
    elif .head != $head then "it names the head \(.head | tojson), not \($head)"
    elif .jobs != $jobs then "it names the Jobs \(.jobs | tojson), not \($jobs | tojson)"
    elif (.medians | type) != "object" or any(.medians[]; type != "object") then
      "its medians are no object of fixtures, each an object of filters"
    else empty end
  end' "$recorded" 2>&1)" || recorded_fault "jq reads no JSON in it: $fault"
[ -z "$fault" ] || recorded_fault "$fault"
# One line per recorded pair: the fixture, the filter and the median as the file writes it, or the
# JSON type of a value that is no number.
pairs="$(jq -r '.medians | to_entries[] | .key as $fixture | .value | to_entries[]
  | [$fixture, .key, (if (.value | type) == "number" then .value | tostring else "a " + (.value | type) end)]
  | @tsv' "$recorded")"
printf 'c-tree-read-bound: the recorded medians of head %s and the Jobs %s: %s\n' \
  "$recorded_head" "$recorded_jobs" "$recorded"

# The fixtures and the filters of the test's lines (skeletonShapes' labels and skeletonFilters'
# names), each line one pair.
fixtures="largest-plan most-comments most-blocks largest-spec"
filters="no-view kind spec verification plan decisions"

RECORDED_PAIRS="$pairs" awk -v fixtures="$fixtures" -v filters="$filters" -v recorded_file="$recorded" -v head="$recorded_head" '
function numeric(value) {
  return value ~ /^[0-9]+(\.[0-9]+)?$/
}
function median(a, b, c,    swap) {
  if (a > b) { swap = a; a = b; b = swap }
  if (b > c) { swap = b; b = c; c = swap }
  if (a > b) { swap = a; a = b; b = swap }
  return b
}
BEGIN {
  entries = split(ENVIRON["RECORDED_PAIRS"], entry, "\n")
  for (e = 1; e <= entries; e++) {
    split(entry[e], part, "\t")
    recorded[part[1] " " part[2]] = part[3]
  }
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
  for (pair in recorded) {
    if (!(pair in expected)) {
      split(pair, part, " ")
      printf "c-tree-read-bound: fixture=%s read=%s: %s records a median of a pair of no fixture and filter the check names\n", part[1], part[2], recorded_file
      bad = 1
    }
  }
  for (i = 1; i <= named; i++) {
    for (k = 1; k <= read; k++) {
      pair = fixture[i] " " filter[k]
      judged = 1
      for (job = 1; job <= 3; job++) {
        if (count[job, pair] == 0) {
          printf "c-tree-read-bound: fixture=%s read=%s: %s holds no old/new pair\n", fixture[i], filter[k], name[job]
          judged = 0
        } else if (count[job, pair] > 1) {
          printf "c-tree-read-bound: fixture=%s read=%s: %s holds %d pairs in one log, want one\n", fixture[i], filter[k], name[job], count[job, pair]
          judged = 0
        } else if (!numeric(old[job, pair]) || !numeric(new[job, pair])) {
          printf "c-tree-read-bound: fixture=%s read=%s: %s holds an unmeasured pair, old=%s new=%s\n", fixture[i], filter[k], name[job], old[job, pair], new[job, pair]
          judged = 0
        }
      }
      if (!(pair in recorded)) {
        printf "c-tree-read-bound: fixture=%s read=%s: %s records no median of it\n", fixture[i], filter[k], recorded_file
        judged = 0
      } else if (!numeric(recorded[pair])) {
        printf "c-tree-read-bound: fixture=%s read=%s: %s records %s as its median, not a number of milliseconds\n", fixture[i], filter[k], recorded_file, recorded[pair]
        judged = 0
      }
      if (!judged) {
        bad = 1
        continue
      }
      before = median(old[1, pair] + 0, old[2, pair] + 0, old[3, pair] + 0)
      now = median(new[1, pair] + 0, new[2, pair] + 0, new[3, pair] + 0)
      recorded_ms = recorded[pair] + 0
      within = now <= 2 * recorded_ms || (recorded_ms < 2 && now <= recorded_ms + 1)
      verdict = "within the bound"
      if (!within) {
        verdict = "over the bound: the new median is above twice the recorded median, and not within 1 ms above a recorded median under 2 ms"
        bad = 1
      }
      printf "c-tree-read-bound: fixture=%s read=%s: old median %.3f ms (%s, %s, %s), new median %.3f ms (%s, %s, %s), recorded median %.3f ms: %s\n", \
        fixture[i], filter[k], before, old[1, pair], old[2, pair], old[3, pair], now, new[1, pair], new[2, pair], new[3, pair], recorded_ms, verdict
    }
  }
  if (bad) {
    print "FAIL: tree-read-db-medians.sh: a pair is missing, duplicated, unmeasured, unrecorded or over the bound of its recorded median, each printed above"
    exit 1
  }
  printf "PASS: tree-read-db-medians.sh complete: %d fixtures and %d filters, each new median within the bound of its recorded median of head %s\n", named, read, substr(head, 1, 10)
}
' "$@"
