#!/usr/bin/env bash
# The live proof that each phase of an issue leaves a step record on its Dispatch issue, and that a
# person reads the records back with `dispatch steps`. The driver
# (packages/daemon/internal/daemon/step_records_live_test.go, which this script builds with
# `-tags e2e`) builds and starts the real Dispatch server from this checkout's packages/envoy on a
# database of its own, boots the whole daemon with dispatch_url at it, walks one issue from
# planning to done with one failed review, and reads the records with the real `dispatch` CLI
# (packages/envoy-client/bin/dispatch.ts under bun) as a person of the project, with the personal
# token Dispatch minted for that person. Each check prints `== <name>`, what it observed and
# `ok <name>`; the first that fails ends the run non-zero, naming it:
#   - walk-to-the-second-review-with-dispatch-up: planning, implementing and testing complete, a
#     person's review requests changes, and implementing and testing complete again; each of those
#     completions reads its handoff record from Dispatch, so Dispatch is up for them;
#   - a-phase-moves-while-dispatch-is-down: with the Dispatch process stopped the second review
#     approves, the architect signs off, the retro completes and GitHub merges the pull request, so
#     the issue reaches verifying, where its close waits for the read of its checks from Dispatch;
#     control: `dispatch steps` then exits non-zero naming the unreachable server, and the daemon's
#     outbox holds the issue's step records unfinished; Dispatch started on the same database, the
#     records of those four steps reach it, each ended, within the outbox's longest backoff and one
#     lease, and the read of the issue's checks then closes it, done, since the project runs no
#     production check and the issue has no check to wait for;
#   - walk-to-done-with-one-failed-review: `dispatch steps --issue <KEY>` prints the header
#     `<KEY>: 10 steps in 1 tasks` and one line for each run (steps 1 to 5, the review and the
#     implementing and testing before it twice, the retro and the merge), each ended with its
#     result, each with an agent, each of kind step with the model that answered; control: the
#     failed review's record deleted from Dispatch's database (its node and its tuples), the same
#     parse reports `4 step reviewer review attempt 1 … failed` missing;
#   - a-write-that-names-another-session-is-refused: a step write sent with the agent's bearer
#     and the actor ses_step_agent is refused 403 STEP_WRITER and leaves no record; control: the
#     same body with the daemon's own credential and the actor legion-daemon:<PROJECT> is taken.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/step-records.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. STEP_RECORDS_EVIDENCE_DIR (default a fresh directory under TMPDIR,
# or /tmp when it is unset, kept and printed) keeps the transcript, the nats-server's log, the
# daemon's, Dispatch's (dispatch.log) and the driver's output. Nothing reaches github.com or a
# deployed Dispatch. A run takes two to five minutes, most of it building Dispatch and the driver,
# and up to a minute and a half waiting for the outbox's backoff after the outage. The run around
# the driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
require_dispatch_cli
live_daemon_proof 'step records' step-records TestStepRecordsLive "${STEP_RECORDS_EVIDENCE_DIR:-}"
