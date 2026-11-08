#!/usr/bin/env bash
# The one retry every download in this repository's CI goes through: a workflow step, a composite
# action, or a script under .github/scripts/. No site holds a copy of this loop
# (dispatch://INFRA-1545, R2) — a site that needs a retry sources this file and calls the function
# it defines. A Dockerfile is the one exception, and not because of this file: it can carry no new
# file above a layer whose order it sets on purpose, so each of its network RUN lines opens with
# one canonical inline definition of the same loop instead.
#
# This file is SOURCED, never run. It defines one function, `retry_fetch`, and does nothing else:
# no option is set, no variable is left behind, nothing is printed. From a workflow `run:`, whose
# working directory is the repository root:
#
#     . .github/scripts/ci-retry.sh
#     retry_fetch "the Bun toolchain" 180 -- bun install --frozen-lockfile
#
# From a composite action, whose `run:` also starts at the caller's workspace rather than at the
# action's own directory, so the path to this file goes through $GITHUB_ACTION_PATH:
#
#     . "$GITHUB_ACTION_PATH/../../scripts/ci-retry.sh"
#     retry_fetch "changie" 60 -- curl -fsSLo changie.tar.gz "$url"
#
# A sourced file that ran `set -euo pipefail` would set those options on the step that sourced it,
# and a step that never asked for them would then end at its first failing command. So this one
# sets nothing, and `retry_fetch` is written to be correct both under a caller that has errexit
# and pipefail on — a failing attempt must not kill that caller before the retry happens — and
# under one that has neither. The shebang is here for shellcheck and for the reader; nothing
# execs this file.

# retry_fetch <what> <per-attempt bound in seconds> -- <command...>
#
# Runs <command...> under `timeout <bound>` until it succeeds, at most five times, sleeping 10,
# 20, 40 and 80 seconds between the attempts. The fifth attempt therefore starts 150 seconds after
# the first on the sleeps alone, which meets R1's floor — at least five attempts, the last starting
# at least 120 seconds after the first — whatever each attempt itself costs. A successful attempt
# returns at once and sleeps nothing, so the cost of this wrapper on a healthy download is the
# `timeout` process and nothing else.
#
# Each attempt is bounded because the fault this answers is not only a refusal: a host that accepts
# the connection and then sends nothing would hold one attempt for as long as the job's own budget,
# and the retries would never happen. The bound is per attempt, measured from that attempt's start.
#
# <what> is a short phrase naming what is being downloaded ("the Bun toolchain", "changie"); it
# opens every line this function prints, so a log says which download is retrying. <command...> is
# executed by `timeout` directly, so it must be a program with its arguments — not a shell
# function, a pipeline, or a redirection.
#
# Every line this function writes goes to standard error. The command's own standard output is
# buffered: each attempt writes into a temporary file of its own, and only a successful attempt's
# file is copied to standard output, byte for byte. A failed attempt's bytes reach neither
# stream, and one line on standard error says how many were withheld. So a caller may capture the
# value:
#
#     names=$(retry_fetch "the run's artifact list" 30 -- gh api "$route" --jq '.[].name')
#
# The buffer is what makes that capture safe, and gh is the measurement behind it: gh copies an
# error answer's body to standard output as it copies a good one's, `--jq` included. Measured
# with gh 2.98.0, a 422 prints {"message":"No commit found for SHA: …","status":"422"} there and
# exits 1, and a GraphQL error prints {"data":…,"errors":[…]} the same way. Without the buffer, a
# retry that recovered would hand its caller the refused attempt's bytes in front of the good
# answer, and a caller parsing that value would read two documents or the wrong one.
#
# The two sites that capture therefore need no wrapper of their own: dequeued.yaml's read of a
# pull request's timeline, whose value decides whether the poll goes round again, and
# main-bypass-check.yaml's read of the head's ci-ok check runs, which `jq --argjson` then parses.
# A caller that redirects the call into a file needs none either: the redirection is the caller's
# own, it is applied once, and nothing of a failed attempt reaches it.
#
# The command's own standard error is left alone. It goes straight to the caller's, failed
# attempt or not, because that is where a reader finds out what the host said.
#
# The runner reads a step's standard error for workflow commands as it reads its standard output,
# so the `::error::` line below still becomes the step's annotation.
#
# On exhaustion it prints one `::error::` line, which GitHub turns into the step's annotation, and
# returns the last attempt's exit status.
retry_fetch() {
  local what bound attempts attempt status left noun refusal buffer withheld unit

  if [ "$#" -lt 3 ]; then
    echo "::error::retry_fetch takes <what> <per-attempt bound in seconds> -- <command...>, and got $# argument(s)" >&2
    return 2
  fi
  what=$1
  bound=$2
  shift 2
  if [ "$1" != "--" ]; then
    echo "::error::retry_fetch: no -- separator before the command; it takes <what> <per-attempt bound in seconds> -- <command...>" >&2
    return 2
  fi
  shift
  refusal="::error::retry_fetch: the per-attempt bound is a positive whole number of seconds, not '$bound'"
  case $bound in
    '' | *[!0-9]*)
      echo "$refusal" >&2
      return 2
      ;;
  esac
  if [ "$bound" -eq 0 ]; then
    echo "$refusal" >&2
    return 2
  fi
  if [ "$#" -eq 0 ]; then
    echo "::error::retry_fetch: nothing to run after --; it takes <what> <per-attempt bound in seconds> -- <command...>" >&2
    return 2
  fi

  attempts=5
  attempt=1
  while :; do
    # Each attempt's standard output goes into a file of its own, and only a successful attempt's
    # file is copied out. The note above says why: an attempt that fails can still have written a
    # body, and a caller that captures this call would otherwise hold it in front of the answer
    # the retry fetched.
    #
    # No `trap … EXIT` removes these files. This function is sourced, so such a trap would be the
    # caller's own: it would fire at the end of the caller's shell and replace whatever trap the
    # caller had already set, which is how a second `trap … EXIT` once discarded the removal of
    # ci-ok-report.sh's work directory. Each file is removed by name instead, on every path out
    # of this loop. An interrupt between the `mktemp` and the `rm` leaves one buffer in
    # ${TMPDIR:-/tmp}, which on a runner is the job's own temporary directory and goes with the
    # job.
    buffer=$(mktemp "${TMPDIR:-/tmp}/ci-retry.XXXXXX") || buffer=
    if [ -z "$buffer" ]; then
      echo "::error::retry_fetch: no temporary file for $what's output could be made in ${TMPDIR:-/tmp}" >&2
      return 2
    fi
    # `|| status=$?` rather than `if ! timeout ...`: under a caller's errexit a bare failing
    # command would end the step here, before this function could retry anything.
    status=0
    timeout "$bound" "$@" > "$buffer" || status=$?
    if [ "$status" -eq 0 ]; then
      cat "$buffer"
      rm -f "$buffer"
      return 0
    fi
    # Counted, never guessed, and counted before the file goes. BusyBox's `wc` pads the number
    # with spaces where GNU's does not, so the padding comes off; `|| withheld=0` keeps a `wc`
    # that somehow failed from ending a caller that has errexit on.
    withheld=$(wc -c < "$buffer" | tr -d ' ') || withheld=0
    rm -f "$buffer"
    left=$((attempts - attempt))
    if [ "$left" -eq 1 ]; then noun=attempt; else noun=attempts; fi
    # 124 is the status GNU coreutils' `timeout` returns when it kills the command, and it is the
    # only status this says reached the bound. BusyBox's `timeout`, which the Alpine-based images
    # run, exits 143 instead — the signal, not GNU's distinct code — and 143 is also what any
    # command killed by SIGTERM from elsewhere exits with, so neither it nor any other status can
    # be read as this bound's own kill.
    if [ "$status" -eq 124 ]; then
      echo "$what: attempt $attempt of $attempts reached its ${bound}s bound (exit 124); $left $noun left" >&2
    else
      echo "$what: attempt $attempt of $attempts failed (exit $status); $left $noun left" >&2
    fi
    # Only where the attempt wrote something. A host that refuses the connection sends no body,
    # which is most of what this helper meets, and a line of zeros beside each of five attempts
    # would say nothing.
    if [ "$withheld" -gt 0 ]; then
      if [ "$withheld" -eq 1 ]; then unit=byte; else unit=bytes; fi
      echo "$what: attempt $attempt of $attempts wrote $withheld $unit to standard output; none of it is printed" >&2
    fi
    if [ "$attempt" -ge "$attempts" ]; then
      echo "::error::$what failed to download after $attempts attempts (last exit $status)" >&2
      return "$status"
    fi
    sleep "$((10 << (attempt - 1)))"
    attempt=$((attempt + 1))
  done
}
