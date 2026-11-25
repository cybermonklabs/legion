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
# This file is SOURCED, never run. It sources ci-log-guard.sh, the file beside it (below), and
# defines one function of its own, `retry_fetch`, and does nothing else: no option is set, no
# variable is left behind, nothing is printed but the one refusal below. From a workflow `run:`,
# whose working directory is the repository root:
#
#     . .github/scripts/ci-retry.sh
#     retry_fetch "the Bun toolchain" 180 -- bun install --frozen-lockfile >&2
#
# A CALLER WHOSE COMMAND CAN WRITE A CREDENTIAL TO STANDARD OUTPUT MUST NOT GO THROUGH THIS
# FUNCTION, because a failed last attempt's standard output reaches the step log, and so does a
# successful one's where the caller sends it to standard error. No caller does today: the `gh`
# ones pass their token in the environment, the leak engine and render-nats write with
# `--output` and `-o`, crane prints digests and configuration, and the registry pull token goes
# through `ci_tools_curl` (review rounds 21 and 22 of dispatch://INFRA-1545 read every one of
# them). A fetch of a minted token is the shape to keep out: a body that times out part way is
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

# The stop-commands block each attempt runs inside is ci-log-guard.sh's, which INFRA-5224's lane
# sources too, so the tree holds one copy of that block (dispatch://INFRA-1545, round 54). It is
# read from this file's own directory, which bash gives a sourced file in BASH_SOURCE whatever
# path the caller wrote. dash gives none: it sets no BASH_SOURCE, and a sourced file has no other
# way to find its own path. So a caller that runs under dash sources ci-log-guard.sh itself, just
# before this file, and this file then finds ci_log_block_open defined and reads nothing:
# scripts/go-third-party-notices.sh (a `#!/bin/sh` script) and dispatch-browser.yaml's shard
# and alone jobs (`sh -e {0}` in their container) do. A caller that does neither, or a copy of
# this file with no ci-log-guard.sh beside it, gets one `::error::` naming that rule, and the
# source returns 2, so a `sh -e` step stops at it rather than at dash's bare "can't open".
# BASH_SOURCE is written with no index: bash reads the bare name as element 0, this file, and
# dash reads `${BASH_SOURCE[0]}` as a bad substitution wherever it expands it.
if ! command -v ci_log_block_open > /dev/null 2>&1; then
  if [ -n "${BASH_SOURCE-}" ] && [ -r "$(cd "$(dirname "$BASH_SOURCE")" && pwd)/ci-log-guard.sh" ]; then
    # shellcheck source=.github/scripts/ci-log-guard.sh
    . "$(cd "$(dirname "$BASH_SOURCE")" && pwd)/ci-log-guard.sh"
  else
    echo "::error::ci-retry.sh: ci_log_block_open is not defined and no ci-log-guard.sh can be read beside this file; a caller whose shell has no BASH_SOURCE (dash, sh) must source .github/scripts/ci-log-guard.sh before ci-retry.sh" >&2
    return 2
  fi
fi

# retry_fetch <what> <per-attempt bound in seconds> -- <command...>
#
# Runs <command...> under `timeout <bound>` until it succeeds, at most five times, sleeping 10,
# 20, 40 and 80 seconds between the attempts. The fifth attempt therefore starts 150 seconds after
# the first on the sleeps alone, which meets R1's floor — at least five attempts, the last starting
# at least 120 seconds after the first — whatever each attempt itself costs. A successful attempt
# returns at once and sleeps nothing, so the cost of this wrapper on a healthy download is one
# `timeout` process, one `mktemp`, one read of 16 bytes of /dev/urandom for the attempt's block,
# the attempt's output written to that file and copied out of it with `cat`, and one `rm` — a
# buffering round trip rather than the process alone, which is the price of a caller being able
# to capture the value safely (the paragraph on the buffer below).
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
# file is copied out, byte for byte: to standard output, as the caller's value, or inside the
# attempt's block where the caller sent standard output to standard error (the paragraph on the
# block below). A copy that cannot be written — no space on the device, an I/O error, a reader
# that closed the pipe — is this call's status, so a cut answer never reads as a success. A
# failed attempt's bytes reach neither stream, and one line on standard error says how many were
# withheld — EXCEPT THE LAST ATTEMPT'S, which nothing retries after and which is the diagnosis:
# its bytes are copied to STANDARD ERROR, cut at 4096 bytes, with one line naming the whole size
# and the part printed. Standard output still carries a successful attempt's bytes or nothing at
# all, so a caller may capture the value:
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
# The command's own standard error is not buffered. It goes straight to the caller's, failed
# attempt or not, because that is where a reader finds out what the host said. It is the host's
# text all the same, and the runner acts on a workflow command in it as in standard output: it
# ends a line at a lone carriage return as at a newline, reads as a command a line that starts
# with `::` once its leading spaces and tabs are gone, and reads `##[<command>]` anywhere in a line
# (ci-log-guard.sh's header cites the runner's code). go, npm, gh, git and crane write a server's
# error body there (dispatch://INFRA-5224/artifact/server-text-audit-md). SO EACH ATTEMPT RUNS
# INSIDE A STOP-COMMANDS BLOCK OF ITS OWN, ci-log-guard.sh's: `ci_log_block_open` writes
# `::stop-commands::<token>` on standard error before the command starts, with a token
# `ci_log_token` reads fresh for that attempt from /dev/urandom, which no host sees before the
# block opens, and `ci_log_block_close` writes a newline and `::<token>::` once the command has
# ended (dispatch://INFRA-1545, round 54). Between the two the runner acts on no workflow command.
# Both markers go on standard error, the stream the command's text is on: the runner keeps one
# stop state for the whole step, reads the two streams on two threads and handles each batch's
# standard error lines first, so a block on standard output around text on standard error does
# not hold. The newline puts the close at the start of a line even where the command's last line
# had none; where it had one, the log shows an empty line.
#
# Inside the block, after the command, goes a failed last attempt's standard output (below), or a
# successful attempt's where the caller sent standard output to standard error,
# `retry_fetch … >&2`, which this function reads as `[ /dev/fd/1 -ef /dev/fd/2 ]`: both
# descriptors name one file. After the close go this function's own lines, each attempt's report
# and the `::error::` below, so the `::error::` still becomes the step's annotation; and a
# successful attempt's standard output where the caller's standard output is anything else — a
# capture, a file, a pipe — copied plain, since a marker there would be part of the caller's
# value.
#
# On exhaustion it prints one `::error::` line, which GitHub turns into the step's annotation, the
# last attempt's own standard output on standard error, inside that attempt's block, before its
# close, and returns that attempt's exit status.
#
# AN ATTEMPT LOG, FOR A RIG THAT COUNTS THE ATTEMPTS. Where CI_RETRY_ATTEMPT_LOG names a file,
# each attempt appends one line to it as it ends, `attempt <n> exit <status>`: its number and its
# command's exit status, 0 for the attempt that succeeded. Nothing else this function prints says
# which attempt succeeded, and the download-retry job's recover runs must know it: each sets the
# variable for its site (through `start --attempt-log`, a verb and option of
# .github/scripts/download-retry-proof.sh) and its report refuses a call that passed only on its
# last attempt. .github/scripts/ci-tools.sh's ci_tools_curl writes the same lines for the
# attempts curl makes itself. Unset or empty, nothing is written. A line that cannot be written
# leaves bash's own refusal on standard error and changes nothing else, so the call still
# returns the command's status.
#
# THREE STATED LIMITS. First, a successful attempt's standard output that the caller neither
# captures nor sends to standard error reaches the log outside any block, as the command printed
# it, so a caller whose command prints on success sends it with `>&2`. No call of the tree is left
# so: each one captures its value, sends it to a file, /dev/null or a pipe, ends in `>&2` (the
# calls review round 54 of dispatch://INFRA-1545 changed), or runs a command that prints nothing
# on standard output when it succeeds (`go mod download`, `go install`, `gh api --silent`,
# `gh cache delete`, `crane copy`, `curl --silent --output`). Second, a caller that captures an
# answer and prints a value out of it prints it outside any block: check-pr-text.sh's lines that
# print the base branch's name, `$base_ref` (GitHub's GraphQL baseRefName since main's #4956):
# the refusal `answered no page $broken of the commits from $base_ref`, the notice `GraphQL
# comparison of $base_ref with $head`, `whole history "commits on $base_ref …"` (in the two
# refusals of `whole`), the notice `no commit on $base_ref adds $cutoff_file` and the two
# `unknown`s on `$base_ref's history`; its `unknown` `no 200 status line` (the release read's
# status line); its `unknown` `$tag was published at $published` and its notice `active since
# $first_tag, published $first_published` (a release's published_at); and its notice `#$number
# was opened at $opened` (the pull request's createdAt); ci-ok-reconcile.sh's lines in its
# `while read -r id status was marker` loop that print `$status`, `$was` or `$marker` (a ci-ok
# check run's status, conclusion and external_id); ci-ok-report.sh's notice `its base $BASE_SHA
# is not $tip` (the tip of the queue's branch, main's #4983); ci-ok-hold.sh's notices `This run
# was created at $created` (this run's creation time), `$HEAD_REF is ${group:-gone}` (the group
# branch's commit) and `its base $BASE_SHA is not $tip` (the tip of the queue's branch, main's
# #5186); dequeued.yaml's lines `left the queue at … for reason '$reason'` (the removal's createdAt
# and reason) and `What that commit's runs are` (the group head's runs, as jq's compact JSON);
# mirror-ci-images.sh's check_go, in its refusal and its `go` notice (the GOLANG_VERSION an
# image's config holds), and its refusal `landed at $have` (the digest crane read);
# post-check-run.sh's notice `posted $CHECK_NAME …` (the check run's html_url);
# browser-image.yaml's line `publish=$publish: $why` in `Decide whether to publish` (the recipe
# label an image's config carries); dispatch-browser-fetch-binaries.sh's notice `artifact $name,
# id $id, … $size bytes, digest …`, its refusal that the artifact `id $id` has expired and its
# last refusal's `of $size bytes` (the run's artifact list, main's #5144); and
# main-bypass-check.yaml's command `cat "$RUNNER_TEMP/reasoning" >&2` (main-bypass-check.sh's
# lines, which name the conclusions of the runs and check runs it read, escaped by jq's @tsv).
# Those lines are the callers' own, outside this function: the boundary of the INFRA-3862 lane
# (dispatch://INFRA-5224). Third, the callers' rule: the runner keeps one stop state for the
# whole step and reads standard output and standard error on two threads, so a workflow command
# a caller writes on STANDARD OUTPUT beside a call of this function, just before it or just
# after it, can be read while one of the call's blocks is open, and dropped: 32 of 50 just after
# a call, measured by the INFRA-3862 flakes peer with the runner's own ProcessInvoker and
# ActionCommandManager, and 15 of 30 just before one, on GitHub's own runner (the tester's run
# 45074154101 of dispatch://INFRA-1545). So every workflow command a caller prints in the same
# step as a call of this function, before it or after it, goes to standard error, where it stays
# in order with the blocks, as this function's own lines do.
#
retry_fetch() {
  local what bound attempts attempt status left noun refusal buffer withheld unit token
  # How much of the last attempt's output is printed. A failed download writes a body of any size
  # — a proxy's HTML page, a registry's JSON, apt's whole progress stream — and this goes into a
  # log a person reads, so the report is cut and says that it cut it. The command's own standard
  # error is not cut, because this function never buffers it. A local, so that sourcing this file
  # still leaves no variable behind; the harness writes the same 4096 as a literal.
  local final_output_bytes=4096

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
    # The attempt runs inside a stop-commands block of its own (the header's paragraph on the
    # command's standard error says why), with a token ci_log_token reads fresh for it. A token
    # that cannot be made is refused as a buffer that cannot be made is, before the command runs,
    # and ci_log_token's own line above the `::error::` says what failed. `|| token=` keeps that
    # failure from ending a caller that has errexit and pipefail on: review round 22 of
    # dispatch://INFRA-1545 caught a `/dev/urandom` read whose pipeline status WAS the
    # assignment's status under such a caller, which died there, losing the `::error::` line and
    # the command's own exit status.
    token=$(ci_log_token) || token=
    if [ -z "$token" ]; then
      rm -f "$buffer"
      echo "::error::retry_fetch: no stop-commands token for $what's attempt $attempt could be made, so the command did not run" >&2
      return 2
    fi
    ci_log_block_open "$token"
    # `|| status=$?` rather than `if ! timeout ...`: under a caller's errexit a bare failing
    # command would end the step here, before this function could retry anything or close the
    # block.
    status=0
    timeout "$bound" "$@" > "$buffer" || status=$?
    if [ -n "${CI_RETRY_ATTEMPT_LOG:-}" ]; then
      printf 'attempt %s exit %s\n' "$attempt" "$status" >> "$CI_RETRY_ATTEMPT_LOG" || true
    fi
    if [ "$status" -eq 0 ]; then
      # The copy is the caller's answer, so its status is this call's: a full disk, an I/O error
      # or a reader that closed the pipe cuts the answer short, and `cat` says so. Without this
      # `retry_fetch … -- printf payload > /dev/full` printed `cat: write error` and returned 0.
      # `|| status=$?` rather than a bare `cat`, for the reason the line above gives. Where the
      # caller sent standard output to standard error, the copy is text for the log, the host's
      # as much as the command's standard error is, so it goes inside the block; anywhere else
      # it is the caller's value and goes after the close, with no marker in it.
      if [ /dev/fd/1 -ef /dev/fd/2 ]; then
        cat "$buffer" || status=$?
        ci_log_block_close "$token"
      else
        ci_log_block_close "$token"
        cat "$buffer" || status=$?
      fi
      rm -f "$buffer"
      return "$status"
    fi
    # Counted, never guessed, and counted before the file goes. BusyBox's `wc` pads the number
    # with spaces where GNU's does not, so the padding comes off; `|| withheld=0` keeps a `wc`
    # that somehow failed from ending a caller that has errexit on.
    withheld=$(wc -c < "$buffer" | tr -d ' ') || withheld=0
    # The last attempt's bytes are the diagnosis, and nothing retries after it, so they are
    # printed — on standard error, never on standard output, because a caller may be capturing
    # this call's value and a failed attempt's body must never reach it (the paragraph on the
    # buffer above). An outage of apt's mirrors reported `677 bytes, none printed` on every one of
    # fifteen attempts across three jobs of this repository's own CI on 2026-11-11, five to a job
    # (jobs 130594958298, 130594957391 and 130660138851),
    # and a reader diagnosing it could not see which host or which error apt had named
    # (dispatch://INFRA-1545). At most `final_output_bytes` of them: a failed download can write a
    # body of any size, and a log is read by a person. They are bytes a host chose, so they are
    # printed before this attempt's block closes, where the runner treats every line as text, a
    # body's own `::add-mask::` or `::stop-commands::` included (review round 21 of
    # dispatch://INFRA-1545, which found no exploit path and asked for the door shut anyway). The
    # line that introduces them opens with a newline, as the close does, so it starts a line of
    # its own after a command whose standard error ended without one.
    if [ "$attempt" -ge "$attempts" ] && [ "$withheld" -gt 0 ]; then
      if [ "$withheld" -gt "$final_output_bytes" ]; then
        printf '\n%s\n' "$what: the last attempt wrote $withheld bytes to standard output; the first $final_output_bytes of them follow on standard error" >&2
      else
        printf '\n%s\n' "$what: the last attempt wrote $withheld bytes to standard output; they follow on standard error" >&2
      fi
      # `|| true`: a read of this file that fails must not end a caller that has errexit on, and
      # the status this call returns is the command's own rather than this report's.
      head -c "$final_output_bytes" "$buffer" >&2 || true
    fi
    ci_log_block_close "$token"
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
    rm -f "$buffer"
    if [ "$attempt" -ge "$attempts" ]; then
      echo "::error::$what failed to download after $attempts attempts (last exit $status)" >&2
      return "$status"
    fi
    sleep "$((10 << (attempt - 1)))"
    attempt=$((attempt + 1))
  done
}
