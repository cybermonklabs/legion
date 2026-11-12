#!/usr/bin/env bash
# The retry a download of this repository's CI goes through when it fetches with a command of its
# own: a workflow step, a composite action, or a script. No such site holds a copy of this loop
# (dispatch://INFRA-1545, R2) — it sources this file and calls the function it defines.
# TWO OTHER MECHANISMS RETRY THE REST, and this file is not the whole of R2:
# .github/scripts/ci-tools.sh's own `ci_tools_curl` family carries the ladder for every asset of
# .github/ci-tools.yaml, with the per-ladder arithmetic its own block on `--retry-max-time`
# derives; and a Dockerfile carries an inline copy, because it can add no new file above a layer
# whose order it sets on purpose, so each of its network RUN lines opens with one canonical
# inline definition of the same loop.
#
# This file is SOURCED, never run. It defines one function, `retry_fetch`, and does nothing else:
# no option is set, no variable is left behind, nothing is printed. From a workflow `run:`, whose
# working directory is the repository root:
#
#     . .github/scripts/ci-retry.sh
#     retry_fetch "the Bun toolchain" 180 -- bun install --frozen-lockfile
#
# A CALLER WHOSE COMMAND CAN WRITE A CREDENTIAL TO STANDARD OUTPUT MUST NOT GO THROUGH THIS
# FUNCTION, because a failed last attempt's standard output reaches the step log. No caller does
# today: the `gh` ones pass their token in the environment, the leak engine and render-nats write
# with `--output` and `-o`, crane prints digests and configuration, and the registry pull token
# goes through `ci_tools_curl` (review rounds 21 and 22 of dispatch://INFRA-1545 read every one
# of them). A fetch of a minted token is the shape to keep out: a body that times out part way is
# printed here, and no mask covers bytes the runner has never seen.
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
# returns at once and sleeps nothing, so the cost of this wrapper on a healthy download is one
# `timeout` process, one `mktemp`, the attempt's output written to that file and copied out of it
# with `cat`, and one `rm` — a buffering round trip rather than the process alone, which is the
# price of a caller being able to capture the value safely (the paragraph on the buffer below).
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
# file is copied to standard output, byte for byte. A copy that cannot be written — no space on
# the device, an I/O error, a reader that closed the pipe — is this call's status, so a cut
# answer never reads as a success. A failed attempt's bytes reach neither stream, and one line on
# standard error says how many were withheld — EXCEPT THE LAST ATTEMPT'S, which nothing retries
# after and which is the diagnosis: its bytes are copied to STANDARD ERROR, cut at
# 4096 bytes, with one line naming the whole size and the part printed. Standard
# output still carries a successful attempt's bytes or nothing at all, so a caller may capture the
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
# On exhaustion it prints one `::error::` line, which GitHub turns into the step's annotation, the
# last attempt's own standard output on standard error, and returns that attempt's exit status.
#
retry_fetch() {
  local what bound attempts attempt status left noun refusal buffer withheld unit
  # How much of the last attempt's output is printed. A failed download writes a body of any size
  # — a proxy's HTML page, a registry's JSON, apt's whole progress stream — and this goes into a
  # log a person reads, so the report is cut and says that it cut it. The command's own standard
  # error is not cut, because this function never buffers it. A local, so that sourcing this file
  # still leaves no variable behind; the harness writes the same 4096 as a literal.
  local final_output_bytes=4096 stop_commands

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
    # ${TMPDIR:-/tmp}, which on a hosted runner is /tmp: nothing there sets TMPDIR, and the job's
    # own temporary directory is $RUNNER_TEMP, which this does not use. The runner's machine is
    # discarded after the job, so the buffer goes with it either way.
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
      # The copy is the caller's answer, so its status is this call's: a full disk, an I/O error
      # or a reader that closed the pipe cuts the answer short, and `cat` says so. Without this
      # `retry_fetch … -- printf payload > /dev/full` printed `cat: write error` and returned 0.
      # `|| status=$?` rather than a bare `cat`, for the reason the line above gives.
      cat "$buffer" || status=$?
      rm -f "$buffer"
      return "$status"
    fi
    # Counted, never guessed, and counted before the file goes. BusyBox's `wc` pads the number
    # with spaces where GNU's does not, so the padding comes off; `|| withheld=0` keeps a `wc`
    # that somehow failed from ending a caller that has errexit on.
    withheld=$(wc -c < "$buffer" | tr -d ' ') || withheld=0
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
    if [ "$withheld" -gt 0 ] && [ "$attempt" -lt "$attempts" ]; then
      if [ "$withheld" -eq 1 ]; then unit=byte; else unit=bytes; fi
      echo "$what: attempt $attempt of $attempts wrote $withheld $unit to standard output; none of it is printed" >&2
    fi
    # The last attempt's bytes are the diagnosis, and nothing retries after it, so they are
    # printed — on standard error, never on standard output, because a caller may be capturing
    # this call's value and a failed attempt's body must never reach it (the paragraph on the
    # buffer above). An outage of apt's mirrors reported `677 bytes, none printed` on every one of
    # fifteen attempts across three jobs of this repository's own CI on 2026-11-11, five to a job
    # (jobs 130594958298, 130594957391 and 130660138851),
    # and a reader diagnosing it could not see which host or which error apt had named
    # (dispatch://INFRA-1545). At most `final_output_bytes` of them: a failed download can write a
    # body of any size, and a log is read by a person.
    if [ "$attempt" -ge "$attempts" ] && [ "$withheld" -gt 0 ]; then
      if [ "$withheld" -gt "$final_output_bytes" ]; then
        echo "$what: the last attempt wrote $withheld bytes to standard output; the first $final_output_bytes of them follow on standard error" >&2
      else
        echo "$what: the last attempt wrote $withheld bytes to standard output; they follow on standard error" >&2
      fi
      # These are bytes a host chose, and the runner reads its workflow commands (`::add-mask::`,
      # `::stop-commands::` and the rest) on standard error as it reads them on standard output.
      # So the copy is wrapped in a stop token the body cannot have guessed: between the two
      # lines below the runner treats every line as text (review round 21 of
      # dispatch://INFRA-1545, which found no exploit path and asked for the door shut anyway).
      # The token is this attempt's own buffer name, which `mktemp` made of the six random
      # characters it chose above and which the body never sees. It needs no command of its own
      # and no fallback: the buffer is non-empty by the time this line runs, because a `mktemp`
      # that failed returned 2 from this function long before it (review round 22, which caught
      # a `/dev/urandom` read whose pipeline status, under a caller that has errexit and pipefail
      # on, WAS the assignment's status - so that caller died here, losing the `::error::` line
      # and the command's own exit status, and the line meant to cover it was unreachable).
      stop_commands=${buffer##*/}
      echo "::stop-commands::$stop_commands" >&2
      # `|| true`: a read of this file that fails must not end a caller that has errexit on, and
      # the status this call returns is the command's own rather than this report's.
      head -c "$final_output_bytes" "$buffer" >&2 || true
      echo >&2
      echo "::$stop_commands::" >&2
    fi
    rm -f "$buffer"
    if [ "$attempt" -ge "$attempts" ]; then
      echo "::error::$what failed to download after $attempts attempts (last exit $status)" >&2
      return "$status"
    fi
    sleep "$((10 << (attempt - 1)))"
    attempt=$((attempt + 1))
  done
}
