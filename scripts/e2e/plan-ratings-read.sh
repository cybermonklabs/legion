#!/usr/bin/env bash
# The read, after the merge, of the plan and the ratings of one issue a deployed Legion ran to
# done: it runs `dispatch plans --project <PROJECT> --since 1970-01-01T00:00:00Z` once, prints what
# the command printed, and judges the line of <ISSUE>. The explicit window lists every finished
# issue of the project, so an issue with no plan and no rating is listed and its faults named:
# with no `--since` the command lists only the newest issues that have a plan or a rating.
#
#   scripts/e2e/plan-ratings-read.sh INFRA INFRA-123
#
# It exits 0 and prints `PASS: …`, with the issue's plan, ratings, time and count of failed steps,
# and its variant where its line names one, only when the read lists <ISSUE> with a plan, at least
# one rating, each rating with a reason, and a count of failed steps. A time the command prints as
# `-` is Dispatch knowing no span of the issue's step records (no record has a start, none has an
# end, or the latest end is before the earliest start), which says nothing of the plan or its
# ratings: the verdict names that time `unknown` and judges the line by the rest. The other issues
# of the project are not judged: a finished issue Legion never ran, or ran before the release that
# rates each plan, lists `plan none` and `ratings none`, which is true of what Dispatch holds of
# it. It exits 1 and prints one `FAIL: …` line per fault when the read lists finished issues and
# <ISSUE>'s line does not hold, or lists no line of <ISSUE>. It exits 2 and prints
# `UNKNOWN: <reason>` when it could read nothing to judge: the read lists no finished issue at all,
# the command failed, or its answer is no read of the project; so an empty or failed read is never
# a pass. A usage error is exit 2 too, with the usage on stderr.
#
# It runs where the operator runs `dispatch` by hand, with the operator's own credential: the
# `dispatch` command on PATH (packages/envoy-client's bin), the session the operator's shell names
# (DISPATCH_HOST and DISPATCH_SESSION_ID), and the deployed Dispatch and token the operator's
# envoy.json or DISPATCH_URL and DISPATCH_TOKEN name. This script sets none of those and reads
# no credential of its own. It reaches only that Dispatch, and writes nothing. It reads no file but
# lib/dispatch-read.sh beside it, so a copy of the two runs outside a checkout of this repository
# too.
set -euo pipefail

usage="usage: scripts/e2e/plan-ratings-read.sh <PROJECT> <ISSUE>, a project key and the key of one issue of it, such as INFRA INFRA-123"
if [ "$#" -ne 2 ] || ! [[ $1 =~ ^[A-Z][A-Z0-9]*$ ]] || ! [[ $2 =~ ^$1-[0-9]+$ ]]; then
  echo "$usage" >&2
  exit 2
fi
project=$1
issue=$2
case ${BASH_SOURCE[0]} in */*) here=${BASH_SOURCE[0]%/*} ;; *) here=. ;; esac
# shellcheck source-path=SCRIPTDIR source=lib/dispatch-read.sh
. "$here/lib/dispatch-read.sh"
read_require_dispatch

read_dispatch plans --project "$project" --since 1970-01-01T00:00:00Z

# Each line after the header is one finished issue, as `dispatch plans` prints it
# (packages/envoy-client/src/dispatch-commands/plans.ts):
#   <issue> closed <closed_at> plan <none|<source> (<n> tasks: <steps|->)> ratings <none|<r>/<most> "<reason>", …> time <-|<h>h<m>m> failed steps <n>[ variant <experiment>/<variant>]
# The line ends with ` variant <experiment>/<variant>` where the records of the issue's newest run
# name an experiment, and at `failed steps <n>` where they name none. Each reason is a JSON string,
# so a quotation mark or a line break in it keeps the issue on one line; the ratings are separated
# by `, ` outside those strings.
awk -v project="$project" -v issue="$issue" '
  function unknown(why) { print "UNKNOWN: " why; status = 2; exit }
  # ratingsOf reads the ratings text of one line: the number of ratings it lists, 0 for `none`,
  # and -1 when it is no list of ratings: each is a whole number, `/`, the top of the scale and a
  # JSON string. Dispatch refuses a rating off the scale, so the number is read as the form alone.
  # It sets reasonless to how many of them have an empty reason.
  function ratingsOf(text,   n, at, head, end, c) {
    reasonless = 0
    if (text == "none") return 0
    n = 0
    at = 1
    while (1) {
      if (!match(substr(text, at), /^[0-9]+\/[0-9]+ "/)) return -1
      head = RLENGTH
      end = at + head
      while (1) {
        c = substr(text, end, 1)
        if (c == "") return -1
        if (c == "\\") { end += 2; continue }
        if (c == "\"") break
        end++
      }
      n++
      if (end == at + head) reasonless++
      end++
      if (end > length(text)) return n
      if (substr(text, end, 2) != ", ") return -1
      at = end + 2
    }
  }
  NR == 1 {
    if ($0 !~ ("^" project ": [0-9]+ finished issues$")) unknown("the first line of the answer is no header of " project ": " $0)
    listed = $2 + 0
    if (listed == 0) unknown("dispatch plans lists no finished issue of " project)
    next
  }
  NF == 0 { next }
  {
    line = $0
    if ($2 != "closed" || $4 != "plan" || !match(line, / time (-|[0-9]+h[0-9]+m) failed steps [0-9]+( variant [a-z0-9-]+\/[a-z0-9-]+)?$/)) unknown("the line is no finished issue: " line)
    tail = substr(line, RSTART + 1)
    lead = length($1 " closed " $3 " plan ")
    middle = substr(line, lead + 1, RSTART - lead - 1)
    # middle is `<plan> ratings <ratings>`: the plan is `none` or `<source> (<n> tasks: …)`, and
    # ` ratings ` (9 characters) ends it.
    if (!match(middle, /^(none|(default|owner) \([0-9]+ tasks: [^)]*\)) ratings /)) unknown("the line is no finished issue: " line)
    plan = substr(middle, 1, RLENGTH - 9)
    ratings = substr(middle, RLENGTH + 1)
    rated = ratingsOf(ratings)
    if (rated < 0) unknown("the line is no finished issue: " line)
    finished++
    if ($1 != issue) next
    found++
    split(tail, parts, " ")
    spent = parts[2]
    failed = parts[5]
    if (plan == "none") faults[++count] = issue " lists no plan: " line
    if (rated == 0) faults[++count] = issue " lists no rating: " line
    if (reasonless) faults[++count] = issue " lists a rating with no reason: " line
    if (spent == "-") spent = "unknown"
    said = "plan " plan ", ratings " ratings ", time " spent ", failed steps " failed
    if (parts[7] != "") said = said ", variant " parts[7]
  }
  END {
    if (status) exit status
    if (finished != listed) { print "UNKNOWN: the header says " listed " finished issues and " finished " lines of issues follow it"; exit 2 }
    if (found == 0) faults[++count] = "the read lists " listed " finished issues of " project " and no line of " issue
    if (found > 1) faults[++count] = "the read lists " issue " " found " times"
    for (i = 1; i <= count; i++) print "FAIL: " faults[i]
    if (count) exit 1
    print "PASS: " issue ": " said
  }
' <<<"$read_out"
