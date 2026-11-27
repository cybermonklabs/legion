#!/usr/bin/env bash
# The live proof that a parent starts no phase while a Dispatch child of it is open
# (dispatch://INFRA-3320's check k14, R9 to R12): a root implementing in its slot, whose architect
# releases a child under it with release_children, stands in awaiting_children at the next
# admission pass, in admission.blocked naming the child and not in admission.active; its
# implementer, in the middle of the phase, gets one message on its own Envoy role topic naming the
# child and saying the phase ended, and its architect one awaiting-children notice; the
# implementer's completion of the ended phase is refused HANDOFF_NOT_CURRENT_PHASE and the root
# stays where it is; and once the child's pull request has merged and the child is done, the root
# stands in implementing again, and its implementer gets a start whose task names the child that
# merged and asks for no merge of the default branch. The whole daemon is booted by the function
# `legion start` calls, over a real PostgreSQL and a real nats-server, and driven only through
# Dispatch's and GitHub's envelopes on the ENVOY_NOTIFICATIONS stream, the daemon's own HTTP
# routes and its worker stream, which the driver speaks as `legion worker-shim`
# does; every step is read back from GET /legion/v1/state, from what the daemon wrote to the
# Dispatch stand-in, from what it published to the Envoy listener stand-in and from what it sent
# the implementer's own connection (packages/daemon/internal/daemon/parent_waits_live_test.go,
# which this script builds with `-tags e2e`). Each check prints `== <name>`, what it observed and
# `ok <name>`; the first that fails ends the run non-zero, naming it.
#
# Run it from a checkout on a Linux host with go, awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/parent-waits.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none. That server must be this machine's: prerequisites reads
# the DSN with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. The broker is the run's own: a
# nats-server from LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install`
# into a GOBIN of its own, started with JetStream on a port of the run's own and stopped on any
# exit. PARENT_WAITS_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log, the daemon's own and the
# driver's output. It needs no kube context, no tmux, no agent and no GitHub, so it runs in a
# Legion pod, in CI and on a devbox alike. With record.ChildOpen made to answer false for every
# child, the run fails at the driver's second step, since no message reaches the implementer's
# role topic after the release: that is k14's negative control. The run around the driver is
# lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'parent waits' parent-waits TestParentWaitsLive \
  "${PARENT_WAITS_EVIDENCE_DIR:-}"
