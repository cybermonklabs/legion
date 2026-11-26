#!/usr/bin/env bash
# The live proof that a person turns admission by signals on and sets each limit while the daemon
# runs, and that the daemon keeps each change (dispatch://INFRA-5899, check c-live): with the
# merge queue's wait over its limit and the rule on, a P0 issue starts as it does with the rule off
# and the two issues below P0 wait, `legion state --json` names the signal that holds them with its
# value and its limit, and the daemon's log has the line of each start the signal held; a lower
# limit, still under the value, holds the first of them again at its own pass (the negative
# control); the limit raised above the value starts it with no restart while the second stays
# behind its Dispatch link; a restart over the same store reads back the switch, the limit, who set
# them, why and every entry of the record of changes; a limit outside its signal's range is refused
# with its reason and changes nothing; a limit cleared while the rule is on takes that limit alone
# away; and with the rule off the cap starts the second issue once its link is gone, though the
# queue still reads over its limit. The recording Dispatch stand-in refuses any write of an
# admission reading, and the proof holds that none came. The whole daemon is booted by the function
# `legion start` calls, over a real PostgreSQL and a real nats-server, driven by the `legion`
# binary this checkout builds and by the Dispatch envelopes of the ENVOY_NOTIFICATIONS stream, and
# the merge queue's wait comes from a recorded GitHub timeline its GitHub stand-in serves, never
# from a value (packages/daemon/internal/daemon/admission_signals_live_test.go, which this script
# builds with `-tags e2e`). Each check prints `== <name>`, what it observed and `ok <name>`; the
# first that fails ends the run non-zero, naming it.
#
# Run it from a checkout on a Linux host with go, git, jj, awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/admission-signals.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none, so the tester can re-run it wherever one is. That server
# must be this machine's: prerequisites reads the DSN with lib/loopback-dsn.sh's
# require_loopback_dsn and refuses a host that is not 127.0.0.1, localhost or ::1 with exit 2,
# before it builds or starts anything. The broker is the run's own: a nats-server from
# LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install` into a GOBIN of
# its own, started with JetStream on a port of the run's own and stopped on any exit.
# ADMISSION_SIGNALS_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log, the daemon's own, the
# driver's output, and each `legion state --json` answer a step was judged on, the negative
# control's two among them (state-*-negative-control-held.json and
# state-*-negative-control-raised.json); the run's work directory, the driver binary among it, is
# under TMPDIR too, since a pod's own disk is small and its TMPDIR names the issue's volume. The run
# around the driver is lib/live-daemon-proof.sh's, which sub-architect.sh makes too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'admission signals' admission-signals TestAdmissionSignalsLive \
  "${ADMISSION_SIGNALS_EVIDENCE_DIR:-}"
