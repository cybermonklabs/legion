#!/usr/bin/env bash
# The live proof that a completion the `legion` tool sends after the issue moved on is answered for
# the phase the role left (dispatch://INFRA-3146): a reviewer whose own approval moved the issue to
# accepting before it completed has its completion taken, naming its own review record, and a
# finished tester that completes again, naming its own test record, is refused
# `409 HANDOFF_NOT_CURRENT_PHASE` in one request. The whole daemon is booted by the function
# `legion start` calls, over a real PostgreSQL, a real nats-server and the real Dispatch server
# this checkout builds, and two real `omp --mode rpc` panes run under it — the real
# `legion worker-shim`, this checkout's `legion`, and this checkout's @sjawhar/pi-envoy and
# @sjawhar/pi-legion in an Oh My Pi profile of the run's own — each answered by an HTTP model
# stand-in of the driver's own that calls the `legion` tool's `handoff_complete` as a model does,
# with a `tool_use` block, so every completion is the request the shipped plugin builds.
#
# One issue stands in testing on an open pull request. The tester's pane takes its task, the
# driver writes its test record to Dispatch as that pane's own Oh My Pi session, and its turn
# completes testing with verdict pass, which moves the issue to reviewing. The operator's own task
# (`legion claims deliver`) gives the finished tester a second turn, which calls `handoff_complete`
# again. The reviewer the pass started takes its reviewing task, and its turn is held while the
# driver writes its review record as its own session and publishes the review App's approval of
# the head, its Legion footer naming that session, as Envoy relays a GitHub review; the daemon
# moves the issue to accepting, and the turn then completes. Each step records the tool's answer,
# which comes back to the stand-in as the tool's result, and the daemon's own line for the one
# request each call made (`api: handoff completion answered`, which names the record the call
# carried and the status and code it was answered with); the last step holds the repeat refused
# `409 HANDOFF_NOT_CURRENT_PHASE` naming the test record, and the late completion taken naming the
# review record, which GET /legion/v1/state shows on the reviewer's phases row. The driver is
# packages/daemon/internal/daemon/late_completion_live_test.go, which this script builds with
# `-tags e2e`, over packages/daemon/internal/daemon/localruntime_live_test.go, the runtime that
# starts a pane with no tmux. Each check prints `== <name>`, what it observed and `ok <name>`; the
# first that fails ends the run non-zero, naming it.
#
# The whole run took 135 s measured in a Legion worker pod (omp/18.16.0) with a warm Go build
# cache: 61 s building the driver and 67 s of proof, of which building Dispatch was 12 s and the
# four steps 17 s. The negative control, the same run with packages/pi-shared/src/handoff-record.ts
# and the completion route's choice of the phase and its record at 14a2ca93, took 127 s and
# failed in step 4 on both late answers, each `400 HANDOFF_RECORD_REQUIRED` naming no record. The
# proof waits on no clock of its own beyond the 3 s it watches after each completion for a second
# request.
#
# Run it from a checkout on a Linux host with go, awk, bash, tar, bun, jj and python3, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp \
#     LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/late-completion.sh
# LEGION_OMP_PATH is required and names the Oh My Pi every pane runs, an absolute executable: this
# proof launches real agents, and no pin is guessed for it. In a Legion worker pod that binary is
# the fleet's build rather than the branch's fetch, and the driver prints which build it ran.
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on, and on
# which the driver makes Dispatch's database, so its role may create databases and the server
# carries pgvector, as CI's service, a devbox container and a pod's own embedded PostgreSQL do on
# a loopback address; this script starts none. That server must be this machine's: prerequisites
# reads the DSN with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not
# 127.0.0.1, localhost or ::1 with exit 2, before it builds or starts anything.
# The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one this
# script installs with `go install` into a GOBIN of its own, started with JetStream on a port of
# the run's own and stopped on any exit. LATE_COMPLETION_EVIDENCE_DIR (default a fresh directory
# under TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript, the
# nats-server's log, the daemon's own, Dispatch's, each pane's and the driver's output; the run's
# work directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is small
# and its TMPDIR names the issue's volume. The driver builds `legion` and Dispatch and packs the
# two plugins from this checkout itself, so it needs bun and the repository's node_modules, and
# the panes' working directory is a jj repository it makes with jj. It needs no kube context, no
# Claude Code, no GitHub and no tmux — the panes are child process groups of the driver — so it
# runs in a Legion pod and on a devbox alike. The run around the driver is
# lib/live-daemon-proof.sh's, which role-prompt-rule.sh and handoff-records.sh make too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
if [ -z "${LEGION_OMP_PATH:-}" ]; then
  echo "FAIL setup: LEGION_OMP_PATH is required: it names the Oh My Pi every pane of this proof runs (an absolute executable path). This proof launches real agents and guesses no build." >&2
  exit 1
fi
if [ ! -x "$LEGION_OMP_PATH" ]; then
  echo "FAIL setup: LEGION_OMP_PATH ($LEGION_OMP_PATH) is not an executable file" >&2
  exit 1
fi
for tool in bun jj; do
  command -v "$tool" >/dev/null || {
    echo "FAIL setup: $tool is not on PATH; the driver packs the plugins with bun and makes the panes' jj workspace with jj" >&2
    exit 1
  }
done
[ -d "$root/node_modules" ] || {
  echo "FAIL setup: $root/node_modules is missing; run \`bun install\` at the checkout's root first" >&2
  exit 1
}
# The driver binary runs from this directory, not the package's, so it is told the checkout it
# builds Dispatch from.
export LEGION_E2E_REPO_ROOT="$root"
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'late completion' late-completion \
  TestLateCompletionsThroughTheRealLegionToolLive \
  "${LATE_COMPLETION_EVIDENCE_DIR:-}"
