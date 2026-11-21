#!/usr/bin/env bash
# The live proof that the work of a phase entry reaches the worker that entry names
# (dispatch://INFRA-2800's R3, R4, R5, R6 and R9). The driver
# (packages/daemon/internal/daemon/phase_entry_converge_live_test.go, which this script builds with
# `-tags e2e`) boots the whole daemon on the PostgreSQL LEGION_TEST_PG_DSN names and the
# nats-server this run starts, holds it to six wakes a minute in bursts of one, and drives each
# role over the daemon's own worker stream as `legion worker-shim` does. Each check prints
# `== <name>`, what it observed and `ok <name>`; the first that fails ends the run non-zero,
# naming it:
#   - run one, in three checks: the boot's relaunch of the implementer spends the burst, so the
#     daemon's wake pacing has not let the tester's launch run and that role's start waits for it,
#     its claim queued entry 1's task; the pace then lets the launch run while the implementer is
#     still in the turn its completion was made from, so the tester comes up holding that task
#     unsent, since the daemon sends no role a task that adopts the issue's working copy while
#     another role of the issue is in a turn; the issue's phase changes twice while the claim
#     holds it — the tester asks for the work back with its own backward move, from a turn of its
#     own, and the implementer, still in its turn, pushes a second head and completes there — which
#     raises the tester's phases row to entry 2 and writes that entry's own task; once both turns
#     end the tester is sent the task it held, the completion it reports for that work is refused
#     409 HANDOFF_STALE_ENTRY naming both entries and recording nothing, and the entry the phase is
#     on then reaches the same connection as a task of its own, whose completion the daemon takes;
#   - run two: the driver holds the advisory lock a lane takes before it claims a lifecycle row,
#     drives the reviewer's own backward move to implementing, which writes the implementer's next
#     start row, and deletes that row before any lane has run it; with no fact of the project
#     arriving after it, the daemon's own entry sweep sends that entry's own task to the
#     implementer's live connection. The sweep's period is a Go constant with a test override the
#     driver sets (convergeEntrySweep); the shipped binary keeps entrySweepInterval's minute;
#   - one read of GET /legion/v1/state, which carries the issue's `phaseEntry` and `givenEntry`.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/phase-entry-converge.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the run
# creates a database of its own there. That server must be this machine's: prerequisites reads the
# DSN with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database and no container. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN,
# or the pinned one installed with `go install`. PHASE_ENTRY_CONVERGE_EVIDENCE_DIR (default a fresh
# directory under TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript, the
# nats-server's log, the daemon's and the driver's output. Nothing reaches github.com or a deployed
# Dispatch: the run is self-contained. The proof itself waits out one paced launch, one outbox retry
# backoff and one sweep. The run around the driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
# Twice the longest green run on record, rounded up to the minute: 21 green e2e.yaml jobs (17 of
# them in green runs) up to 2026-11-15, the longest proof step 190 s (run 43679368771).
proof_guard 420
live_daemon_proof 'phase entry converge' phase-entry-converge TestAPhaseEntryConvergesOnARunningDaemonLive \
  "${PHASE_ENTRY_CONVERGE_EVIDENCE_DIR:-}"
