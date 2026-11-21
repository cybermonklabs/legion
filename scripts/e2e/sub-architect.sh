#!/usr/bin/env bash
# The live proof that every child that starts runs under an architect of its own (INFRA-3915):
# a child's first phase start starts that architect beside its planner, whether the child came in
# by the release_children of the architect above it, by a person's todo under a live tree or by
# the close of the issue it waited on; at its ready the child's architect is told its own issue and
# every issue under it, in a catch-up on its own topic alone, which the daemon logs as it publishes
# it; it files and releases its own children, whose child-status and child-closed notices then
# reach it rather than the root's architect; it reruns only issues under its own and is answered
# 403 ISSUE_NOT_OWNED for its sibling and for the root, and its own backlog in Dispatch stands on
# an issue under its own and is set back on its sibling and on the root; when its claim fails
# because its launches ran out, the root's architect is told and hears the child's next notice too,
# and the child's next phase start retries the claim in the same session with fresh budgets; and a
# child's architect is suspended when the root's architect writes backlog on the child, and resumed
# in its session by the child's rerun. The whole daemon is
# booted by the function `legion start` calls, over a real PostgreSQL and a real nats-server, and
# driven only through the Dispatch envelopes of the ENVOY_NOTIFICATIONS stream, its own HTTP
# routes, its worker stream and the fake runtime's report of a process; every step of its seven is
# read back from GET /legion/v1/state, the operator's claim listing, the notices the listener
# stand-in took on each architect's topic, what the daemon wrote to the Dispatch stand-in, the
# launches the fake runtime was asked for and the daemon's own log
# (packages/daemon/internal/daemon/sub_architect_live_test.go, which this script builds with
# `-tags e2e`). Each check prints `== <name>`, what it observed and `ok <name>`; the first that
# fails ends the run non-zero, naming it.
#
# Run it from a checkout on a Linux host with go, awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/sub-architect.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none, so the tester can re-run it wherever one is. That server
# must be this machine's: prerequisites reads the DSN with lib/loopback-dsn.sh's
# require_loopback_dsn and refuses a host that is not 127.0.0.1, localhost or ::1 with exit 2,
# before it builds or starts anything. The broker is the run's own: a nats-server from
# LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install` into a GOBIN of
# its own, started with JetStream on a port of the run's own and stopped on any exit.
# SUB_ARCHITECT_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is unset,
# kept and printed) keeps the transcript, the nats-server's log, the daemon's own and the driver's
# output; the run's work directory, the driver binary among it, is under TMPDIR too, since a pod's
# own disk is small and its TMPDIR names the issue's volume. The run around the driver is
# lib/live-daemon-proof.sh's, which child-adoption.sh makes too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'sub architect' sub-architect TestEveryChildThatStartsRunsUnderItsOwnArchitectLive \
  "${SUB_ARCHITECT_EVIDENCE_DIR:-}"
