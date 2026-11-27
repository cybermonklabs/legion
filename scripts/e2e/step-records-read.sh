#!/usr/bin/env bash
# The read, after the merge, of the step records of one issue a deployed Legion ran to done: it
# runs `dispatch steps --issue <ISSUE>` once and prints what the command printed, then runs
# `dispatch plans --project <PROJECT>` once, <PROJECT> the key's part before its number, and
# prints the line of <ISSUE> alone, and judges the records by that plan. It reads an issue that is
# done, so every step of it has ended.
#
#   scripts/e2e/step-records-read.sh INFRA-123
#
# It exits 0 and prints `PASS: …` only when the read lists a record of each step of the issue's
# plan, from 1 to its last, in the role the plan names for it (the step's role, before any
# `:key`), a record of the retro and of the merge, every record it lists has ended, every record
# of kind step and of kind retro names an agent, and every record of kind step names the model
# that answered. The steps come from the plan of the issue's newest run, as Dispatch holds it, not
# from the records, so a record lost for the last step fails as one lost for any other. A record
# of a step counts whatever task it names: a run whose task was stopped and replaced keeps records
# of its own. A record of kind merge with no agent passes, with one line that says so: GitHub
# merged the pull request before a merger started, so no merger ran that step, and its record says
# so. It exits 1 and prints one `FAIL: …` line per fault when the read lists records and one of
# those does not hold. It exits 2 and prints `UNKNOWN: <reason>` when it could read nothing to
# judge: the steps read lists no record at all, a command failed, an answer is no read of the
# issue or of its project, or the plan read gives no plan to judge by (it lists no line of the
# issue, so the issue is not done; Dispatch holds no plan of it; or the plan has more than one
# task, of which `dispatch plans` names the first's steps alone); so an empty or failed read is
# never a pass. A usage error is exit 2 too, with the usage on stderr.
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
project=${issue%-*}
case ${BASH_SOURCE[0]} in */*) here=${BASH_SOURCE[0]%/*} ;; *) here=. ;; esac
# shellcheck source-path=SCRIPTDIR source=lib/dispatch-read.sh
. "$here/lib/dispatch-read.sh"
read_require_dispatch
read_dispatch steps --issue "$issue"
steps_out=$read_out
read_dispatch_quiet plans --project "$project"

# Each line after the header is one finished issue, as `dispatch plans` prints it
# (packages/envoy-client/src/dispatch-commands/plans.ts):
#   <issue> closed <closed_at> plan <none|<source> (<n> tasks: <steps of the first|->)> ratings … time … failed steps <n>
# The awk prints the line of the issue, then the steps of its plan, or the reason it cannot.
planned=$(awk -v project="$project" -v issue="$issue" '
  function unknown(why) { print why; status = 2; exit status }
  NR == 1 {
    if ($0 !~ ("^" project ": [0-9]+ finished issues$")) unknown("the first line of the plans answer is no header of " project ": " $0)
    listed = $2 + 0
    if (listed == 0) unknown("dispatch plans lists no finished issue of " project ", so " issue " is not done")
    next
  }
  NF == 0 { next }
  {
    finished++
    if ($1 != issue) next
    found++
    line = $0
    if ($2 != "closed" || $4 != "plan" || !match($0, / plan (none|(default|owner) \([0-9]+ tasks: [^)]*\)) ratings /)) unknown("the line of " issue " is no finished issue: " $0)
    # The plan is the match less ` plan ` (6 characters) before it and ` ratings ` (9) after it.
    plan = substr($0, RSTART + 6, RLENGTH - 15)
  }
  END {
    if (status) exit status
    if (finished != listed) unknown("the plans header says " listed " finished issues and " finished " lines of issues follow it")
    if (found == 0) unknown("dispatch plans lists " listed " finished issues of " project " and no line of " issue ", so it is no issue that is done")
    if (found > 1) unknown("dispatch plans lists " issue " " found " times")
    if (plan == "none") unknown("Dispatch holds no plan of " issue ", so the steps it ran are unknown")
    match(plan, /\([0-9]+ tasks: /)
    tasks = substr(plan, RSTART + 1, RLENGTH - 9) + 0
    steps = substr(plan, RSTART + RLENGTH, length(plan) - RSTART - RLENGTH)
    if (tasks != 1) unknown("the plan of " issue " has " tasks " tasks, and dispatch plans names the steps of the first alone")
    if (steps == "-") unknown("the plan of " issue " names no step")
    print line
    print steps
  }
' <<<"$read_out") || read_unknown "$planned"
printf '%s\n' "${planned%%$'\n'*}"

# The answer is the lines `dispatch steps` prints
# (packages/envoy-client/src/dispatch-commands/steps.ts): a header; where the records of the
# issue's newest run name an experiment, the line `Experiment <experiment>: variant <variant>.`,
# and no such line where they name none; then one run per line, except the line of a task of the
# issue's plan, which comes before its runs and is no run:
#   <task> <step> <kind> <role> <type|-> attempt <a> <agent|-> <model|-> <started_at|-> <ended_at|running> <result|running>
#   task <task> needs <[tasks]|[]|->: <place> <step> <state>, …
# The line `Experiment …` is read only right after the header; anywhere else it is no run.
awk -v issue="$issue" -v steps="${planned#*$'\n'}" '
  function unknown(why) { print "UNKNOWN: " why; status = 2; exit status }
  NR == 1 {
    if ($0 !~ ("^" issue ": [0-9]+ steps in [0-9]+ tasks$")) unknown("the first line of the answer is no header of " issue ": " $0)
    listed = $2 + 0
    if (listed == 0) unknown("dispatch steps lists no record of " issue)
    next
  }
  NF == 0 { next }
  NR == 2 && /^Experiment [a-z0-9-]+: variant [a-z0-9-]+\.$/ { next }
  $1 == "task" && $3 == "needs" { next }
  {
    if (NF != 12 || $6 != "attempt") unknown("the line is no run: " $0)
    runs++
    place = ($3 == "step") ? "step " $2 : "the " $3
    seen[place] = 1
    if ($3 == "step" && !((place, $4) in ran)) {
      ran[place, $4] = 1
      roles[place] = roles[place] (roles[place] == "" ? "" : " ") $4
    }
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
    # Each step of the plan is a role or role:key; its records name the role.
    last = split(steps, planned, " ")
    for (i = 1; i <= last; i++) {
      role = planned[i]
      sub(/:.*/, "", role)
      if (!(("step " i) in seen)) faults[++count] = issue ": no record of step " i " (" planned[i] ")"
      else if (!(("step " i, role) in ran)) faults[++count] = issue ": no record of step " i " (" planned[i] "): its records name " roles["step " i]
    }
    split("the retro,the merge", places, ",")
    for (i = 1; i <= 2; i++) if (!(places[i] in seen)) faults[++count] = issue ": no record of " places[i]
    for (i = 1; i <= noted; i++) print notes[i]
    for (i = 1; i <= count; i++) print "FAIL: " faults[i]
    if (count) exit 1
    print "PASS: " issue ": " runs " records; steps 1 to " last " of its plan (" steps "), the retro and the merge each have one, each record ended, each step and retro with an agent, each step with a model"
  }
' <<<"$steps_out"
