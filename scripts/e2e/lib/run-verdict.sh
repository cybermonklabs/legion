# shellcheck shell=bash
# A proof's last line, and the mark it rests on: one rule for every script under scripts/e2e
# whose EXIT trap prints PASS. Sourced by each of them, never run; it needs nothing of its
# caller's. Every other EXIT trap there that prints a verdict prints FAIL only, from a mark of its
# own, and its script does not source this file.
#
# bash runs the EXIT trap of a shell that a signal ends (a closed pane's HUP, a Ctrl-C's INT or
# timeout's TERM, none of them trapped) with $? at the status of the last command it finished,
# which is often 0. A trap that reads the status alone then prints PASS for a run that never
# reached its last check. So a run passes only when it also reached its end: the script calls
# run_end as the statement after its last check, nothing else sets the mark, and nothing but the
# shell's end runs after it. lib/run-verdict.test.ts holds every call site to that: past each `fi`
# of an `if` around it and each `else` branch, the next line of code is none, the `)` that ends a
# subshell, or an `exit 0`, or else the closing brace of a function each call of which is held to
# the same rule in its own shell. A run that a signal ends never reaches that statement, so the
# mark alone keeps it from passing, and no signal trap is needed for it. A signal trap would make
# a killed run slow to stop: bash runs a trapped signal's handler only once the foreground command
# returns, and a proof runs its driver in the foreground for up to an hour. The scripts that trap
# INT and TERM keep those traps for their own reasons, and pass the signal's status to
# run_verdict.
#
# The mark is the shell's own variable. A subshell starts with the value the shell had when it
# forked, so a run made in a subshell (lib/live-daemon-proof.sh's, inside approved-version-news.sh
# and busy-relaunch-delivery.sh's negative control) sets its own mark, and the script around it
# still needs its own run_end.

# run_ended is the mark: empty until run_end sets it.
run_ended=''

# run_end marks the run as having reached its end. The script calls it as the statement after its
# last check, and nowhere else.
run_end() { run_ended=1; }

# run_passed STATUS answers 0 when a run that exits with STATUS passed: STATUS is 0 and the run
# reached its end. It is the one rule; run_status and run_verdict read it.
run_passed() { [ "$1" -eq 0 ] && [ -n "$run_ended" ]; }

# run_status STATUS prints the status a run that exits with STATUS ends with: STATUS, or 1 for a
# status of 0 that the run did not reach its end with. A trap passes it to a teardown that keeps
# evidence for a failed run (lib/kind-sandbox.sh's kind_sandbox_teardown), so a killed run keeps
# its evidence too.
run_status() {
  if [ "$1" -ne 0 ] || run_passed "$1"; then
    echo "$1"
  else
    echo 1
  fi
}

# run_verdict STATUS PASSED FAILED prints the run's last line and ends the shell. The EXIT trap
# calls it as its last command, with the status the trap was given:
#   - a run that passed (run_passed) prints PASSED on stdout and exits 0;
#   - a run that ended with status 0 before its end prints a line that says so, then FAILED, on
#     stderr, and exits 1;
#   - any other run prints FAILED on stderr and exits with STATUS.
# A shell that a signal ended keeps that signal's status (129, 130 or 143) whatever the trap exits
# with: bash ends it with the signal once the trap returns.
run_verdict() {
  if run_passed "$1"; then
    echo "$2"
    exit 0
  fi
  [ "$1" -ne 0 ] ||
    echo "the run stopped before its end, by a signal or an exit of its own: its status 0 is no pass" >&2
  echo "$3" >&2
  exit "$(run_status "$1")"
}
