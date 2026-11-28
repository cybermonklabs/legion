# shellcheck shell=bash disable=SC2154 # the variables below are the caller's (see the header)
# The one guard and the one wait of a shell test. At this head its one caller is
# packages/envoy/loadtest/cluster/cluster.test.sh; #4437's other shell tests and proofs, and the
# fixed guard and time-left reading they use, come with those files.
#
# Sourced, never run: it defines functions and starts no process. The caller sets
#   work           the run's scratch directory, where each wait keeps its last poll's output
#   timeout_hook   empty or unset, or a function a wait_for that reaches its deadline runs before
#                  it fails
# and defines note, which prints a line of the run's progress, and fail, which prints its message
# and exits.
#
# A script a workflow runs sets its guard once, at its head, with proof_guard_from_env: the CI step
# that runs it exports the deadline from its own timeout-minutes. It waits through wait_for or
# wait_held alone, never through a helper that takes a bound of its own. (wait_held is not
# wait_until: several proofs only a person runs give that name to a bounded wait of their own,
# which a definition here would collide with.)
#
# proof_margin_seconds is how long before the step's limit every wait gives up (the step exports
# its deadline that much early), so the wait's own failure, and the cleanup after it, print before
# GitHub stops the step.
proof_margin_seconds=30
proof_guard_seconds=''
proof_deadline_ms=''
# proof_unguarded is set by proof_guard_from_env where no deadline was exported: each wait then
# ends on its fact alone, and whoever runs the script owns its hang guard.
proof_unguarded=''
# wait_impossible is what a wait's COMMAND sets when it finds that the fact can no longer happen,
# such as the process that would answer having exited.
wait_impossible=''
# wait_gave_up is the line the last wait_held that did not hold ended on, and empty after one
# that held; wait_ended_by is how it ended: `impossible`, or `deadline`.
wait_gave_up=''
wait_ended_by=''
# wait_depth is how many waits are running, one inside another's COMMAND.
wait_depth=0

# proof_now_ms prints the wall clock in epoch milliseconds, the unit LEGION_PROOF_DEADLINE_MS
# carries.
proof_now_ms() {
  local now=${EPOCHREALTIME/[.,]/}
  printf '%s\n' "$((10#$now / 1000))"
}

# proof_guard_from_env sets this proof's guard from the CI step that runs it: the step exports
# LEGION_PROOF_DEADLINE_MS, its own start plus its timeout-minutes less proof_margin_seconds, in
# epoch milliseconds, from the one number its timeout-minutes and STEP_TIMEOUT_MINUTES share, and
# every wait gives up at that moment, naming its fact, before GitHub ends the step. Where it is
# unset or empty (a run on a person's machine, or in any job that sets no such limit), there is no
# wall-time guard: each wait ends on its fact, or at once where its COMMAND set wait_impossible,
# and whoever runs the script owns its hang guard.
proof_guard_from_env() {
  local deadline=${LEGION_PROOF_DEADLINE_MS:-}
  [ -n "${EPOCHREALTIME:-}" ] || fail "proof_guard_from_env: bash 5 or later is required (EPOCHREALTIME)"
  [ -z "$proof_deadline_ms" ] && [ -z "$proof_unguarded" ] || fail "proof_guard_from_env: this proof set its guard already"
  if [ -z "$deadline" ]; then
    proof_unguarded=1
    return 0
  fi
  [[ "$deadline" =~ ^[1-9][0-9]*$ ]] || fail "proof_guard_from_env: LEGION_PROOF_DEADLINE_MS is '$deadline', not a moment in epoch milliseconds"
  proof_deadline_ms=$((deadline + proof_margin_seconds * 1000))
  proof_guard_seconds=$(((proof_deadline_ms - $(proof_now_ms)) / 1000))
  export LEGION_PROOF_DEADLINE_MS=$deadline
}

# wait_held WHAT COMMAND... is the wait of a script that set its guard, for a run that goes on past
# a wait that did not hold and reports it itself; wait_for below is the same wait failing the run.
# It runs COMMAND in this shell every half second until
# it succeeds, and returns 0. It returns 1 at once where COMMAND found the fact can no longer
# happen and set wait_impossible to why, and otherwise, where the proof has a deadline, once
# proof_margin_seconds before the guard's deadline has come, with the line it ended on in
# wait_gave_up; a wait that starts past that moment runs COMMAND once. With no deadline
# (proof_guard_from_env and nothing exported) it ends only on its fact or on wait_impossible.
# It takes no bound and no pace: a step that is only slow never
# fails the run, and a fact that never comes fails it by name. It says how long it has waited
# every 60 s.
#
# COMMAND runs in this shell, so it can set variables and drive a client, and may wait itself.
# Its output goes to $work/wait-for.out, so a COMMAND prints what it read for the failure line
# to show; a wait inside another's COMMAND keeps its own in $work/wait-for.<depth>.out, so it
# never overwrites the output the outer wait reports, and the wait_impossible it reads is its own
# and cleared once read. A poll that never returns would hold the wait past its deadline, so
# every remote call a COMMAND makes carries its own timeout.
wait_held() {
  local what=$1 output polls=0 started=$SECONDS beat=$SECONDS impossible
  shift
  [ -n "$proof_deadline_ms" ] || [ -n "$proof_unguarded" ] ||
    fail "wait_held: this script set no guard (proof_guard_from_env), so the wait for $what has no rule to end by"
  if [ -z "${work:-}" ] || [ ! -d "$work" ]; then
    fail "wait_held: the run has no work directory to keep the output of its wait for $what in"
  fi
  wait_depth=$((wait_depth + 1))
  output=$work/wait-for.out
  ((wait_depth == 1)) || output=$work/wait-for.$wait_depth.out
  while :; do
    polls=$((polls + 1))
    wait_impossible=''
    if "$@" >"$output" 2>&1; then
      wait_depth=$((wait_depth - 1))
      wait_impossible=''
      wait_gave_up=''
      wait_ended_by=''
      return 0
    fi
    impossible=$wait_impossible
    wait_impossible=''
    if [ -n "$impossible" ]; then
      wait_ended_by=impossible
      wait_gave_up="$what: $impossible"
      break
    fi
    if [ -n "$proof_deadline_ms" ] && (($(proof_now_ms) >= LEGION_PROOF_DEADLINE_MS)); then
      wait_ended_by=deadline
      wait_gave_up="gave up waiting for $what after $polls polls, ${proof_margin_seconds}s before this proof's guard of ${proof_guard_seconds}s; its last output: $(tail -n 5 "$output" 2>/dev/null | tr '\n' ' ' || true)"
      break
    fi
    if ((SECONDS - beat >= 60)); then
      beat=$SECONDS
      note "still waiting for $what: poll $polls, $((SECONDS - started))s"
    fi
    sleep 0.5
  done
  wait_depth=$((wait_depth - 1))
  return 1
}

# wait_for WHAT COMMAND... is wait_held failing the run where the wait does not hold: at once,
# naming why, where COMMAND set wait_impossible, and otherwise at the deadline, after
# timeout_hook has run.
wait_for() {
  wait_held "$@" && return 0
  if [ "$wait_ended_by" = deadline ] && [ -n "${timeout_hook:-}" ]; then "$timeout_hook" || true; fi
  fail "$wait_gave_up"
}
