#!/usr/bin/env bash
# The live proof that a reader of the daemon's state can tell a head that stopped from a head that
# is moving: an issue whose review round waits on GitHub's answer about merging its pull request's
# head carries a `mergeWait` object on its view of GET /legion/v1/state, naming the head waited on,
# the merge state the record holds for it, the moment of the daemon's last answering read of it,
# when GitHub last moved the pull request, how many of the fence's check runs are that head's own
# and the head and state the architect and the reviewer were told the round was stuck at; a read
# that only confirms the state already recorded still moves that stamp, which is the whole of what
# tells a head read a minute ago from one nothing has read for a day; a read that changes the
# answer moves both; a push that moves the head, and the approval of the new head that follows it,
# move the object onto that head with no stamp until a read answers for it and the new head's own
# check runs counted; an issue whose round waits on nothing and an issue whose pull request the
# record does not hold open carry no such key at all; and the wait ends when GitHub answers a state
# the issue may advance on. The whole daemon is booted by the function `legion start` calls, over a
# real PostgreSQL and a real nats-server, and driven only through GitHub envelopes on the
# ENVOY_NOTIFICATIONS stream — CI's settlements, the reviews, the push, the synchronize and the
# pushes onto the base that ask the daemon to read a head again; every step is read back from GET
# /legion/v1/state, from the object it decodes to and from the body's own keys, which is what the
# negative control holds (packages/daemon/internal/daemon/merge_wait_readable_live_test.go, which
# this script builds with `-tags e2e`). Each check prints `== <name>`, what it observed and
# `ok <name>`; the first that fails ends the run non-zero, naming it.
#
# Run it from a checkout on a Linux host with go, awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/merge-wait-readable.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none, so the tester can re-run it wherever one is. That server
# must be this machine's: prerequisites reads the DSN with lib/loopback-dsn.sh's
# require_loopback_dsn and refuses a host that is not 127.0.0.1, localhost or ::1 with exit 2,
# before it builds or starts anything. The broker is the run's own: a
# nats-server from LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install`
# into a GOBIN of its own, started with JetStream on a port of the run's own and stopped on any
# exit. MERGE_WAIT_READABLE_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log and the daemon's own; the
# run's work directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is
# small and its TMPDIR names the issue's volume. It needs no kube context, no tmux and no agent, so
# it runs in a Legion pod, in CI and on a devbox alike. The run around the driver is
# lib/live-daemon-proof.sh's, which conflict-no-takeover.sh makes too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'merge wait readable' merge-wait-readable TestTheMergeWaitSaysWhatAReviewRoundWaitsOnFromGitHubLive \
  "${MERGE_WAIT_READABLE_EVIDENCE_DIR:-}"
