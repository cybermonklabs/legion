#!/usr/bin/env bash
# Runs one command against an e2e stack of its own: the fakes, a Dispatch server and its
# database, started here and stopped when the command ends.
#
# `e2e/harness-up.ts` is what starts that stack. It prints the stack's variables on stdout as
# shell exports, the last of them `DISPATCH_E2E_REUSE_SERVERS=1`, and then stays up until it is
# sent a signal, so a caller reads its output down to that line, applies the lines, runs whatever
# addresses the stack, and stops it. This script is those four steps, for a caller that wants one
# command run and nothing else: the acceptance drivers beside it, which speak to a Dispatch server
# over HTTP and through the repository's own `dispatch` command.
#
# Until cybermonklabs/legion#3435 the same thing was written `bun e2e/run.ts -- <command>`; that
# program is gone, and the browser suite starts its stacks through Playwright's own global setup
# now, which no shell driver can reach.
#
# Usage, from `packages/dispatch`:
#
#   DATABASE_URL=<a PostgreSQL this run may truncate> bash e2e/acceptance/with-harness.sh \
#     bash -c 'curl -sS -o /dev/null "http://127.0.0.1:$DISPATCH_E2E_PORT/auth/_dev/signin?login=alice" &&
#       DISPATCH_URL=http://127.0.0.1:$DISPATCH_E2E_PORT DISPATCH_TOKEN=e2e-token \
#       DISPATCH_DEFAULT_ASSIGNEE=alice bash e2e/acceptance/create-document.sh'
#
# The command runs with every variable of the stack exported, `DISPATCH_E2E_PORT` among them, and
# `e2e-token` is the agent bearer the harness pins. The `curl` is the harness's dev sign-in, which
# records `alice` as a person who has signed in, so the project the driver creates may name her as
# its default assignee. The exit status is the command's own, so a pipeline reads it with
# `${PIPESTATUS[0]}` or `set -o pipefail`. Everything this writes goes under `mktemp`, and it
# removes exactly what it made.
set -uo pipefail

if [[ $# -eq 0 ]]; then
  printf 'usage: with-harness.sh <command> [argument…]\n' >&2
  exit 2
fi

exports="$(mktemp)"
errors="$(mktemp)"
harness=""

cleanup() {
  if [[ -n "$harness" ]]; then
    kill "$harness" 2>/dev/null
    wait "$harness" 2>/dev/null
    # A harness that stopped before its stack was up wrote why on its stderr: its start's refusal,
    # or, when a Ctrl-C or this script's caller's limit ended the start, what the start was waiting
    # for, by name (`interruptedBefore`, e2e/local-instance-support.ts). Its waits have no bound of
    # their own, so that is the only account of a process that never came up. Once the stack is
    # up, its stderr is the stack's own output, and nothing of it is printed.
    if ! grep -q '^export DISPATCH_E2E_REUSE_SERVERS=1$' "$exports"; then
      printf 'the harness stopped before it was up:\n' >&2
      cat -- "$errors" >&2
    fi
  fi
  rm -f -- "$exports" "$errors"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

bun e2e/harness-up.ts >"$exports" 2>"$errors" &
harness=$!

# This waits with no deadline of its own, and neither does the harness. `e2e/harness-start.ts`
# waits for each listener's report until that process exits, and fails as soon as it does, so a
# build that failed is reported at once; it sets no bound on a start that is only slow, because
# how long a start takes is the machine's (`serving` in that file says why). A deadline here would
# refuse a harness that is still coming up and hide the harness's own message. The hang guard is
# this script's caller's: a person's Ctrl-C, or the limit of the job that runs it. So the only
# thing this reads is whether the harness is still alive; what it said is printed on the way out,
# by `cleanup`, whichever way this script ends.
until grep -q '^export DISPATCH_E2E_REUSE_SERVERS=1$' "$exports"; do
  if ! kill -0 "$harness" 2>/dev/null; then
    exit 1
  fi
  sleep 1
done

# shellcheck disable=SC1090 # the stack's own exports, written by e2e/harness-up.ts above
. "$exports"

"$@"
