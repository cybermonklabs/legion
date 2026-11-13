#!/usr/bin/env bash
# The live proof of the completion route's entry fence on a completion behind its phase entry: a
# completion of an entry the issue has moved past is taken when the commit it names is the head the
# daemon's record holds, because the phase was entered again at that same head and the work the new
# entry's task asks for is the work that turn did, and it is refused when it names a head the record
# has left or no commit at all, or when the move that entered the phase again moved the issue back,
# where the entry exists to have the work done again and no verdict on either head answers for it.
# Every completion is of the tester's first entry. The tester asks for the work back with its own
# backward move to implementing and ends its turn, since the daemon sends no role a task that adopts
# the issue's working copy while another role of the issue is in a turn; the implementer pushes a
# head of its own and completes, which enters testing again by a forward move while the tester is
# in a turn of its own that no task of the new entry has reached; then the tester's behind
# completion naming the head it tested is answered 409 HANDOFF_STALE_ENTRY naming both heads
# (`stale head refused`), the same completion naming no commit is answered 409 HANDOFF_STALE_ENTRY
# (`behind with no head`), and the same completion naming the head the record holds is answered 200,
# which puts the issue in reviewing (`behind at the head`). The registered reviewer then requests
# the move back to testing, with nothing pushed between, and the same completion at that same head
# is answered 409 HANDOFF_STALE_ENTRY, naming the move back (`behind after a backward move`). Each
# refusal records nothing and leaves the issue in testing. The move back is the real one, over POST
# /legion/v1/phase/backward from a reviewer whose session is registered and holds a grant, because
# the engine takes a backward move from no role but the one running the issue's phase
# (BACKWARD_REFUSED). The proof publishes no CI settlement, because the daemon judges no check and
# moves no issue on a red, and no conflict, because the daemon reads a head's mergeability while the
# tester tests only at its next handoff. The whole daemon is booted by the function `legion start`
# calls, over a real PostgreSQL and a real nats-server, and driven only through GitHub envelopes on
# the ENVOY_NOTIFICATIONS stream, the daemon's own HTTP routes and its worker stream, which the
# driver speaks as `legion worker-shim` does; every step is read back from GET /legion/v1/state and
# from the operator's claim listing, but for the tester's phases row — whether the entry it stands
# on was raised by a move back, and the verdict and commit a refusal leaves unwritten, which no
# route shows and the driver reads from the daemon's own store
# (packages/daemon/internal/daemon/behind_entry_fence_live_test.go, which this script builds with
# `-tags e2e`). Each check prints `== <name>`, what it observed and `ok <name>`; the first that fails
# ends the run non-zero, naming it.
#
# Run it from a checkout on a Linux host with go, awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/behind-entry-fence.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none, so the tester can re-run it wherever one is. That server
# must be this machine's: prerequisites reads the DSN with lib/loopback-dsn.sh's
# require_loopback_dsn and refuses a host that is not 127.0.0.1, localhost or ::1 with exit 2,
# before it builds or starts anything. The broker is the run's own: a nats-server from
# LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install` into a GOBIN of its
# own, started with JetStream on a port of the run's own and stopped on any exit.
# BEHIND_ENTRY_FENCE_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log and the daemon's own; the
# run's work directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is
# small and its TMPDIR names the issue's volume. It needs no kube context, no tmux and no agent, so
# it runs in a Legion pod, in CI and on a devbox alike. The run around the driver is
# lib/live-daemon-proof.sh's, which conflict-no-takeover.sh and child-adoption.sh make too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'behind entry fence' behind-entry-fence TestABehindCompletionIsDecidedByItsHeadAndTheMoveThatRaisedTheEntryLive \
  "${BEHIND_ENTRY_FENCE_EVIDENCE_DIR:-}"
