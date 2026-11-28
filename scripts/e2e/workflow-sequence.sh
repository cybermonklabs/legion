#!/usr/bin/env bash
# The live proof that each issue walks the sequence of phases its record names, on one running
# daemon (dispatch://INFRA-3570): one issue on the library's `default` sequence and one on `light`.
# The driver (packages/daemon/internal/daemon/workflow_sequence_live_test.go, which this script
# builds with `-tags e2e`) boots the whole daemon on the PostgreSQL LEGION_TEST_PG_DSN names and the
# nats-server this run starts, seeds both issues at admitted with their design gates open, and
# drives each role over the daemon's own HTTP routes and worker stream as `legion worker-shim`
# does, a phase of each issue in turn, until both are done. It acts as whatever role the daemon
# starts, at whatever phase the daemon puts an issue in, so the walk it checks is the daemon's
# own. Each check prints `== <name>`, what it observed and `ok <name>`; the first that fails ends
# the run non-zero, naming it:
#   - both issues start: the boot's admission starts the default issue in planning and the light
#     one in implementing, the gate's own row read from each issue's sequence;
#   - both walk to done: the planner's, the implementer's, the tester's (with its verdict) and the
#     reviewer's completions naming their handoff records, the review App's approval of the head
#     as a GitHub envelope, the architect's sign_off where the line has an accepting phase, the
#     implementer's retro, the merger's completion and GitHub's merge; after each move the driver
#     reads each phase worker's phases row and the claims the daemon lists for the issue;
#   - the walks are read from the daemon's own `workflow: phase changed` log lines, which the run
#     keeps in the evidence directory's daemon.log;
#   - the control: each check below is run first against the wrong expectation — the default walk
#     held to the light line and the light walk to the default one, the default issue's first
#     phase held to implementing, its records before its review ended held to no planner, tester
#     or merger started, and at done to the planner and the tester never started — and each must
#     refuse, or the run fails;
#   - the checks: the default issue walked planning, implementing, testing, reviewing, accepting,
#     retro, merging, done; the light issue implementing, reviewing, merging, done, from
#     implementing; at each phase the daemon started the role that works it; the light issue shows
#     no planner, tester or merger before its review ended; and at done the roles never started
#     are none and the planner and the tester;
#   - one line per issue a person can read: its workflow, its phases with the seconds it stood in
#     each, the total and the roles never started.
#
# Run it from a checkout on a Linux host with go, awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/workflow-sequence.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the run
# creates a database of its own there. That server must be this machine's: prerequisites reads the
# DSN with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database and no container. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN,
# or the pinned one installed with `go install`. WORKFLOW_SEQUENCE_EVIDENCE_DIR (default a fresh
# directory under TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript, the
# nats-server's log, the daemon's and the driver's output. Nothing reaches github.com or a deployed
# Dispatch: GitHub is the driver's stand-in for the two pull requests, Dispatch the board the
# whole-daemon drivers share, and no agent runs. The run around the driver is
# lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'workflow sequence' workflow-sequence TestEachIssueWalksItsOwnWorkflowSequenceLive \
  "${WORKFLOW_SEQUENCE_EVIDENCE_DIR:-}"
