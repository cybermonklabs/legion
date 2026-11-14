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
  fi
  rm -f -- "$exports" "$errors"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

bun e2e/harness-up.ts >"$exports" 2>"$errors" &
harness=$!

# This waits with no deadline of its own, because the harness carries better ones. Each listener
# row has its own bound (`e2e/harness-listeners.ts`), ten minutes for the Dispatch server, whose
# `run-server.sh` builds it and whose cold `go build` is the long pole, and thirty seconds for a
# listener that builds nothing; `e2e/harness-start.ts` holds every row to its bound and fails as
# soon as that process exits, so a build that failed is reported at once rather than when a wait
# runs out. A deadline here would be a second, shorter one laid over those, and it would refuse a
# harness that is still coming up and hide the harness's own message. So the only thing this reads
# is whether the harness is still alive.
until grep -q '^export DISPATCH_E2E_REUSE_SERVERS=1$' "$exports"; do
  if ! kill -0 "$harness" 2>/dev/null; then
    printf 'the harness stopped before it was up:\n' >&2
    cat -- "$errors" >&2
    exit 1
  fi
  sleep 1
done

# shellcheck disable=SC1090 # the stack's own exports, written by e2e/harness-up.ts above
. "$exports"

"$@"
