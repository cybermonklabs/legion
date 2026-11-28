#!/usr/bin/env bash
# The live proof that a phase task reaches its role across the adoption of the working copy
# (dispatch://INFRA-3916 R1, R3, R6 and R7). The whole daemon is booted by the function `legion
# start` calls, over a real PostgreSQL and a real nats-server, and four real `omp --mode rpc` panes
# run under it — the real `legion worker-shim`, this checkout's `legion`, and this checkout's
# @sjawhar/pi-envoy and @sjawhar/pi-legion in an Oh My Pi profile of the run's own — against four
# HTTP model stand-ins of the driver's own. Each pane works a real jj workspace, added to a clone
# the driver makes, as a pod's is, and the runtime has the pane's own shim adopt it over the
# claim's worker stream, as the tmux runtime does. Three checks: `stale-adopt`, a workspace another
# workspace made stale, with a changed tracked file and a new file no jj command recorded, is
# brought up to date with `jj workspace update-stale`, the phase task's prompt names the commit
# that holds the two edits, `jj status` there then exits 0 and names no stale working copy, and
# both edits are in that commit; `refused-adoption-resent`, a stale git index.lock in a
# workspace's git directory refuses the adoption more times than prompt_failure_limit, the claim
# keeps its launch and its generation and is charged nothing, the issue's architect gets one
# `worker-stalled` notice, and once the lock is removed the task reaches the stand-in at the
# daemon's next sweep with no message and no operator call; and `ended-turn-holds-nothing`, the
# tester's phase task waits behind the implementer's turn on their shared workspace, and once that
# turn ends in a provider error (a 529) the tester's prompt reaches its stand-in at the next sweep
# (packages/daemon/internal/daemon/adoption_delivery_live_test.go, which this script builds with
# `-tags e2e`, and packages/daemon/internal/daemon/localruntime_live_test.go, the runtime that
# starts a pane with no tmux). Each check prints `== <name>`, what it observed and `ok <name>`; the
# first that fails ends the run non-zero, naming it.
#
# The driver waits out nothing of the provider-error ladder: the one 30 s rung the third check
# arms is what the tester's task must not wait for. Four panes each start Oh My Pi once, and the
# tester's pane starts only once the third check has the implementer in a turn.
#
# Run it from a checkout on a Linux host with go, git, jj (the worker image's, which
# packages/daemon/docker/worker.Dockerfile's ARG JJ_TOOL names: the adoption reads jj's debug
# output, and the daemon's boot refuses to start without jj), awk, bash, tar, bun and python3, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp \
#     LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/adoption-delivery.sh
# LEGION_OMP_PATH is required and names the Oh My Pi every pane runs, an absolute executable: this
# proof launches real agents, and no pin is guessed for it. The driver prints which build it ran.
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none. That server must be this machine's: prerequisites reads
# the DSN with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not
# 127.0.0.1, localhost or ::1 with exit 2, before it builds or starts anything.
# The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned
# one this script installs with `go install` into a GOBIN of its own, started with JetStream on a
# port of the run's own and stopped on any exit. ADOPTION_DELIVERY_EVIDENCE_DIR (default a fresh
# directory under TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript, the
# nats-server's log, the daemon's own, each pane's output and the driver's output; the run's work
# directory, the driver binary and the clones among it, is under TMPDIR too, since a pod's own disk
# is small and its TMPDIR names the issue's volume, and each pane's workspace is in the daemon's
# state directory, which the driver makes as short as a unix socket's path needs. The driver
# builds `legion` and packs the two plugins from this checkout itself, so it needs bun and the
# repository's node_modules. It needs no kube context and no tmux — the panes are child process
# groups of the driver — so it runs in a Legion pod and on a devbox alike, and e2e.yaml runs it on
# a GitHub-hosted runner. The run around the driver is lib/live-daemon-proof.sh's, which
# provider-error-retry.sh makes too.
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
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'adoption delivery' adoption-delivery \
  TestAPhaseTaskReachesItsRoleAcrossTheAdoptionLive \
  "${ADOPTION_DELIVERY_EVIDENCE_DIR:-}"
