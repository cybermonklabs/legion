#!/usr/bin/env bash
# The live proof that a head that starts conflicting with its base keeps the issue in its phase and
# takes no role's turn over (dispatch://INFRA-4374). While the tester works its assignment, a push
# onto the base leaves the head conflicting: the daemon's first read of it is logged and sends
# nothing to any role or its connection; the second read, a minute on, confirms the conflict, and
# since a check the base requires has not passed on the head, the issue stays in testing while the
# implementer is asked on its own Envoy role topic to merge the base forward, the tester is told on
# its own to hold, the architect is told once in a checks-red notice naming testing, and nothing at
# all is sent to the tester's connection, whose turn runs on. Once the implementer's push lands the
# tester is told so; its completion at the head the merge replaced is refused
# HANDOFF_HEAD_MERGED_FORWARD and records nothing, and its completion at the new head is taken.
# The control, a second issue whose required check passed on its conflicting head, gets nothing
# from its second read. The whole daemon is booted by the function `legion start` calls, over a
# real PostgreSQL and a real nats-server, and driven only through GitHub envelopes on the
# ENVOY_NOTIFICATIONS stream, the daemon's own HTTP routes and its worker stream, which the driver
# speaks as `legion worker-shim` does; every step is read back from GET /legion/v1/state, from the
# operator's claim listing, from what the daemon published to the Envoy stand-in, from what it sent
# — or did not send — to a role's own connection and from the lines it logged for its decisions
# (packages/daemon/internal/daemon/conflict_no_takeover_live_test.go, which this script builds with
# `-tags e2e`). Each check prints `== <name>`, what it observed and `ok <name>`; the first that
# fails ends the run non-zero, naming it. The proof waits out the minute between the two reads,
# which no setting shortens, so a run takes more than a minute.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/conflict-no-takeover.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none, so the tester can re-run it wherever one is. That server
# must be this machine's: prerequisites reads the DSN with lib/loopback-dsn.sh's
# require_loopback_dsn and refuses a host that is not 127.0.0.1, localhost or ::1 with exit 2,
# before it builds or starts anything. The broker is the run's own: a
# nats-server from LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install`
# into a GOBIN of its own, started with JetStream on a port of the run's own and stopped on any
# exit. CONFLICT_NO_TAKEOVER_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log and the daemon's own; the
# run's work directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is
# small and its TMPDIR names the issue's volume. It needs no kube context, no tmux and no agent, so
# it runs in a Legion pod, in CI and on a devbox alike. The run around the driver is
# lib/live-daemon-proof.sh's, which child-adoption.sh makes too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'conflict no takeover' conflict-no-takeover TestAConflictKeepsThePhaseAndTakesNoTurnOverLive \
  "${CONFLICT_NO_TAKEOVER_EVIDENCE_DIR:-}"
