#!/usr/bin/env bash
# The live proof that a deployment's plan guidance reaches the launch of an owner and of no other
# agent. The driver (packages/daemon/internal/daemon/plan_guidance_live_test.go, which this script
# builds with `-tags e2e`) builds and starts the real Dispatch server from this checkout's
# packages/envoy on a database of its own, where a person creates one issue in a project of the
# run's own, and boots the whole daemon with dispatch_url at it, over the package's fake runtime,
# which records each launch's spec; the daemon's configuration names a guidance file of planted
# text in plan_guidance. Each check prints `== <name>`, what it observed and `ok <name>`; the first
# that fails ends the run non-zero, naming it:
#   - owner-has-guidance: the issue's architect is launched reading the daemon's copy of the
#     guidance, and its composed prompt holds the copy's header and the planted text after the
#     launch's own addressing;
#   - worker-has-none: the planner, the agent of the issue's first step, is launched with neither;
#   - settings-serve-the-key: GET /legion/v1/state, the route `legion state --json` prints, serves
#     settings.plan_guidance, the guidance file's path;
#   - check-config-names-it: `legion config check`, built from this checkout, on a
#     legion.yaml that names the same file, prints the line that names it.
# The control runs first, as a whole run of its own with its own broker: the same run with
# plan_guidance unset, where owner-has-guidance, settings-serve-the-key and check-config-names-it
# must fail and worker-has-none must hold (TestPlanGuidanceControlLive, each check printed as
# `control-<name>`). It is this script again, run as its own shell with --control, so its run's
# EXIT trap and end mark are that shell's (lib/run-verdict.sh). A control that fails ends the
# script with its status, before the proof's own run starts.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/plan-guidance.sh [--control]
# --control runs the control alone. The script takes no other argument, and refuses one with exit 2.
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. Each run's broker is its own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. PLAN_GUIDANCE_EVIDENCE_DIR (default a fresh directory under TMPDIR,
# or /tmp when it is unset, kept and printed) keeps, under control/ and run/, each run's
# transcript, the nats-server's log, the daemon's, Dispatch's (dispatch.log) and the driver's
# output. Nothing reaches github.com or a deployed Dispatch. The run around each driver is
# lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# The driver binary runs from its run's work directory, not the package's, so it is told the
# checkout it builds Dispatch and the legion binary from.
export LEGION_E2E_REPO_ROOT="$root"
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
case "$*" in
  '' | --control) ;;
  *) refuse "unknown arguments '$*': the one argument this script takes is --control, which runs the control alone" ;;
esac
# The library keeps its own `evidence`, which sourcing it resets, so the run's root is another name.
evidence_root=${PLAN_GUIDANCE_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-plan-guidance-evidence.XXXXXXXX")}
# The control is this script again, as a shell of its own (the header says why), and runs first.
if [ "$*" = --control ]; then
  live_daemon_proof 'plan guidance control' plan-guidance-control TestPlanGuidanceControlLive "$evidence_root/control"
else
  PLAN_GUIDANCE_EVIDENCE_DIR=$evidence_root bash "$0" --control
  live_daemon_proof 'plan guidance' plan-guidance TestPlanGuidanceLive "$evidence_root/run"
fi
