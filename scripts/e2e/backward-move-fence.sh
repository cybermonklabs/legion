#!/usr/bin/env bash
# The live proof that the move which raised a phase entry outranks the heads in the completion
# route's entry fence: a completion of an entry the issue has moved past is taken when the commit
# it names is the head the daemon's record holds, because the phase was entered again at that same
# head and the work the new entry's task asks for is the work that turn did — unless the move that
# entered the phase again moved the issue back, where the entry exists to have the work done again
# and no verdict on either head answers for it. Both readings are driven at one head that never
# moves: the control is the tester's behind completion at that head, answered 200, which puts the
# issue in reviewing; then the registered reviewer requests the move back to testing, with nothing
# pushed between; and the same completion at that same head is answered 409
# HANDOFF_STALE_ASSIGNMENT, with the refusal naming the move back, nothing recorded and the issue
# still in testing. The move back is the real one, over POST /legion/v1/phase/backward from a
# reviewer whose session is registered and holds a grant, because the engine takes a backward move
# from no role but the one running the issue's phase (BACKWARD_REFUSED). The whole daemon is
# booted by the function `legion start` calls, over a real PostgreSQL and a real nats-server, and
# driven only through GitHub envelopes on the ENVOY_NOTIFICATIONS stream, the daemon's own HTTP
# routes and its worker stream, which the driver speaks as `legion worker-shim` does; every step is
# read back from GET /legion/v1/state and from the operator's claim listing, but one — whether the
# entry the tester's row stands on was raised by a move back, which no route shows and the driver
# reads from the daemon's own store (packages/daemon/internal/daemon/backward_move_fence_live_test.go,
# which this script builds with `-tags e2e`). Each check prints `== <name>`, what it observed and
# `ok <name>`; the first that fails ends the run non-zero, naming it.
#
# Run it from a checkout on a Linux host with go, awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/backward-move-fence.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none, so the tester can re-run it wherever one is. That server
# must be this machine's: prerequisites reads the DSN with lib/loopback-dsn.sh's
# require_loopback_dsn and refuses a host that is not 127.0.0.1, localhost or ::1 with exit 2,
# before it builds or starts anything. The broker is the run's own: a nats-server from
# LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install` into a GOBIN of its
# own, started with JetStream on a port of the run's own and stopped on any exit.
# BACKWARD_MOVE_FENCE_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log and the daemon's own; the
# run's work directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is
# small and its TMPDIR names the issue's volume. It needs no kube context, no tmux and no agent, so
# it runs in a Legion pod, in CI and on a devbox alike. The run around the driver is
# lib/live-daemon-proof.sh's, which ci-red-no-takeover.sh and merged-root-waits.sh make too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'backward move fence' backward-move-fence TestABehindCompletionIsRefusedWhenAMoveBackRaisedTheEntryLive \
  "${BACKWARD_MOVE_FENCE_EVIDENCE_DIR:-}"
