#!/usr/bin/env bash
# Sourced by the `.test.sh` harnesses beside it (release-push, check-bun-version): the counters,
# the per-case reporter, and the two predicates they both spell the same way. Each harness keeps
# its own fixtures — what they share is only how a case is reported and counted.
#
# A harness sources this, runs its cases with `check <description> <true|false>`, and ends with
# `summary <what was tested>`, which prints the tally and exits non-zero if anything failed. A
# fact the harness cannot make happen itself, such as a line a detached process writes, it waits
# for through `wait_for`, never through a loop of its own that counts tries or seconds; so does
# ci-tools-proof.sh, the rig of the tool-fallback jobs, which sources this for `wait_for` alone.
#
# Not executable: `source "$(dirname "${BASH_SOURCE[0]}")/test-lib.sh"`.

PASS=0
FAIL=0

check() { # check DESCRIPTION true|false
  local desc="$1" result="$2"
  if [ "$result" = "true" ]; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc"
    FAIL=$((FAIL + 1))
  fi
}

# is <actual> <expected>: true/false for check.
is() { [ "$1" = "$2" ] && echo true || echo false; }

# contains <text> <regex>: true/false for check; greps the whole text.
#
# A here-string, not a pipe. `printf … | grep -q` makes grep exit at the first match while
# printf is still writing, so printf takes SIGPIPE and, under the harnesses' `set -o pipefail`,
# the pipeline reports 141 and a matching text is reported as a MISS. It is a race, so it shows
# up as one assertion failing in maybe one run of six, a different assertion each time, with
# the case count unchanged — which is what makes it look like a mystery rather than a bug.
# Measured on a 4000-line text with a match on line 1: 300/300 spurious with the pipe, 0/300
# with the here-string.
contains() { grep -q -- "$2" <<<"$1" && echo true || echo false; }

# wait_for WHAT COMMAND...: the one wait of a harness, for a fact it cannot make happen itself. It
# runs COMMAND in this shell every tenth of a second until it succeeds, so a COMMAND can set
# variables the harness reads after it. It takes no bound and no pace of a caller's: the guard of a
# harness is the limit of the CI step that runs it, so a step that is only slow never fails a
# case, and a fact that never comes stops the run at that limit. Every 60 s it says what it is
# still waiting for, with the last output of COMMAND, so a run the limit stops names the fact.
wait_for() {
  local what=$1 output polls=0 started=$SECONDS beat=$SECONDS
  shift
  output=$(mktemp)
  until "$@" >"$output" 2>&1; do
    polls=$((polls + 1))
    if ((SECONDS - beat >= 60)); then
      beat=$SECONDS
      echo "  still waiting for $what: poll $polls, $((SECONDS - started))s; its last output: $(tail -n 5 "$output")"
    fi
    sleep 0.1
  done
  rm -f "$output"
}

summary() { # summary WHAT-WAS-TESTED
  echo
  echo "---"
  echo "Results: $PASS passed, $FAIL failed"
  if [ "$FAIL" -gt 0 ]; then
    exit 1
  fi
  echo "PASS: $1"
}
