#!/usr/bin/env bash
# The live proof that a pull request which conflicts with its base once every step of its task has
# run is the merger's to resolve (cybermonklabs/legion#4979), on the default steps, whose last step
# is the acceptance. An issue in accepting whose head GitHub answers CONFLICTING
# has the conflict recorded after the daemon's two reads a minute apart, and nothing is sent to any
# role, the architect's checks-red notice included. The architect's sign-off moves it to retro,
# which asks nothing either; retro's completion moves it to merging, where the merger is started
# and asked on its own Envoy role topic to merge the base forward and resolve the conflict itself,
# the implementer's topic takes no merge-forward message, and the architect is told once in a
# checks-red notice naming merging and the merger. The merger's push of its merge lands, and the
# merger is told to merge only once the reviewer's approval of the new head is on the pull request,
# while the reviewer is asked to read the merge with `git show --remerge-diff`. A required check
# that settles red on the new head is told to the merger and the architect, not to the
# implementer, and GitHub's merge moves the issue to verifying, from which the read of its checks
# after the merge closes it, since the project runs no production check and the issue has no check
# to wait for. No read of the state shows the issue in
# implementing, and the daemon writes no in_progress status. Last, the merger's launch names the
# role prompt parts the daemon composed for a merger and not the driver's own prompt, and the
# headings of each part are printed beside the daemon's messages. The whole daemon is booted by
# the function `legion start` calls, over a real PostgreSQL and a real nats-server, and driven only
# through GitHub envelopes on the ENVOY_NOTIFICATIONS stream, the daemon's own HTTP routes and its
# worker stream, which the driver speaks as `legion worker-shim` does; every step is read back from
# GET /legion/v1/state, from the operator's claim listing, from what the daemon published to the
# Envoy stand-in, from what it sent — or did not send — to a role's own connection, from the
# statuses it wrote to the Dispatch stand-in and from the lines it logged for its decisions
# (packages/daemon/internal/daemon/conflict_merger_live_test.go, which this script builds with
# `-tags e2e`). Each check prints `== <name>`, what it observed and `ok <name>`; the first that
# fails ends the run non-zero, naming it. The proof waits out the minute between the two reads,
# which no setting shortens, so a run takes more than a minute.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/conflict-merger.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none, so the tester can re-run it wherever one is. That server
# must be this machine's: prerequisites reads the DSN with lib/loopback-dsn.sh's
# require_loopback_dsn and refuses a host that is not 127.0.0.1, localhost or ::1 with exit 2,
# before it builds or starts anything. The broker is the run's own: a nats-server from
# LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install` into a GOBIN of its
# own, started with JetStream on a port of the run's own and stopped on any exit.
# CONFLICT_MERGER_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is unset,
# kept and printed) keeps the transcript, the nats-server's log and the daemon's own; the run's work
# directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is small and
# its TMPDIR names the issue's volume. It needs no kube context, no tmux and no agent, so it runs in
# a Legion pod, in CI and on a devbox alike. The run around the driver is
# lib/live-daemon-proof.sh's, which conflict-no-takeover.sh makes too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'conflict merger' conflict-merger TestAConflictPastTheReviewIsTheMergersLive \
  "${CONFLICT_MERGER_EVIDENCE_DIR:-}"
