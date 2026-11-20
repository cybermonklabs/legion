#!/usr/bin/env bash
# legion: tool - reads the step records of one issue a deployed Legion ran to done from that deployment's Dispatch, with the operator's own credential, for the tester who decides the production check of the release that records each step, after the merge.
# The read, after the merge, of the step records of one issue a deployed Legion ran to done: it
# runs `dispatch steps --issue <ISSUE>` once, prints what the command printed, and judges it. It
# reads an issue that is done, so every step of it has ended.
#
#   scripts/e2e/step-records-read.sh INFRA-123
#
# It exits 0 and prints `PASS: …` only when the read lists a record of each of steps 1 to 5, of
# the retro and of the merge, every record it lists has ended, every record of kind step and of
# kind retro names an agent, and every record of kind step names the model that answered. A
# record of kind merge with no agent passes, with one line that says so: GitHub merged the pull
# request before a merger started, so no merger ran that step, and its record says so. It exits 1
# and prints one `FAIL: …` line per fault when the read lists records and one of those does not
# hold. It exits 2 and prints `UNKNOWN: <reason>` when it could read nothing to judge: the read
# lists no record at all, the command failed, or its answer is no read of the issue; so an empty
# or failed read is never a pass. A usage error is exit 2 too, with the usage on stderr.
#
# It runs where the operator runs `dispatch` by hand, with the operator's own credential: the
# `dispatch` command on PATH (packages/envoy-client's bin), the session the operator's shell names
# (DISPATCH_HOST and DISPATCH_SESSION_ID), and the deployed Dispatch and token the operator's
# envoy.json or DISPATCH_URL and DISPATCH_TOKEN name. This script sets none of those and reads
# no credential of its own. It reaches only that Dispatch, and writes nothing.
set -euo pipefail

usage="usage: scripts/e2e/step-records-read.sh <ISSUE>, the key of one issue, such as INFRA-123"
if [ "$#" -ne 1 ] || ! [[ $1 =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]]; then
  echo "$usage" >&2
  exit 2
fi
issue=$1
case ${BASH_SOURCE[0]} in */*) here=${BASH_SOURCE[0]%/*} ;; *) here=. ;; esac
# shellcheck source-path=SCRIPTDIR source=lib/dispatch-read.sh
. "$here/lib/dispatch-read.sh"
read_require_dispatch
read_dispatch steps --issue "$issue"

# Each line after the header is one run, as `dispatch steps` prints it:
#   <task> <step> <kind> <role> <type|-> attempt <a> <agent|-> <model|-> <started_at|-> <ended_at|running> <result|running>
awk -v issue="$issue" '
  function unknown(why) { print "UNKNOWN: " why; status = 2; exit status }
  NR == 1 {
    if ($0 !~ ("^" issue ": [0-9]+ steps in [0-9]+ tasks$")) unknown("the first line of the answer is no header of " issue ": " $0)
    listed = $2 + 0
    if (listed == 0) unknown("dispatch steps lists no record of " issue)
    next
  }
  NF == 0 { next }
  {
    if (NF != 12 || $6 != "attempt") unknown("the line is no run: " $0)
    runs++
    place = ($3 == "step") ? "step " $2 : "the " $3
    seen[place] = 1
    # A run that has ended prints its end, a moment, in column 11; steps.ts prints `running` there
    # (and in column 12) for one that has not, and any other text there is no end either.
    if ($11 !~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]/) faults[++count] = "the record never ended: " $0
    if ($8 == "-" && $3 != "merge") faults[++count] = "the record names no agent: " $0
    if ($8 == "-" && $3 == "merge") notes[++noted] = "the merge has no agent: no merger ran it: " $0
    if ($3 == "step" && $9 == "-") faults[++count] = "the step names no model: " $0
  }
  END {
    if (status) exit status
    if (runs != listed) unknown("the header says " listed " records and " runs " lines of runs follow it")
    split("step 1,step 2,step 3,step 4,step 5,the retro,the merge", places, ",")
    for (i = 1; i <= 7; i++) if (!(places[i] in seen)) faults[++count] = issue ": no record of " places[i]
    for (i = 1; i <= noted; i++) print notes[i]
    for (i = 1; i <= count; i++) print "FAIL: " faults[i]
    if (count) exit 1
    print "PASS: " issue ": " runs " records; steps 1 to 5, the retro and the merge each have one, each record ended, each step and retro with an agent, each step with a model"
  }
' <<<"$read_out"
