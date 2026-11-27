# shellcheck shell=bash
# packages/envoy/scripts/lib/approve-check-steps.sh
#
# How approve-check.sh records its steps and how it ends: one line per step on stdout and in
# $WORK/results.tsv, and an exit status of 1 when a step failed, 3 when none failed and one is
# UNKNOWN, 0 when every step passed. approve-check.test.sh drives these alone, with no stack.
#
# It reads WORK and keeps FAILED and UNKNOWN, which the caller sets to 0 before its first step.

# result <step> <PASS|FAIL|UNKNOWN> <what it read>: one step's line.
result() {
  printf 'approve-check: %s: %s %s\n' "$1" "$2" "$3"
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$WORK/results.tsv"
  case "$2" in
    FAIL) FAILED=1 ;;
    UNKNOWN) UNKNOWN=1 ;;
  esac
}

# stop_unknown <step> <what it read>: a step no later step can run without - a stack that did not
# start, a sign-in or a setup that did not complete - recorded UNKNOWN, and the run ends there as
# finish ends it: 1 where an earlier step failed, 3 where none did.
stop_unknown() {
  result "$1" UNKNOWN "$2"
  finish
}

# finish: the run's end, after its last step.
finish() {
  if [ "$FAILED" = 1 ]; then exit 1; fi
  if [ "$UNKNOWN" = 1 ]; then exit 3; fi
  echo "approve-check: every step passed; results in $WORK/results.tsv" >&2
  exit 0
}
