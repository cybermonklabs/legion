#!/usr/bin/env bash
# The live proof that a later review round is told the commit the review before it read: the task
# the daemon prompts the reviewer with in its second review round holds `Last reviewed commit:
# <sha>.` on its last line, the head the reviewer's own deciding review of the first round named, a review whose
# Legion footer names the entry of that round, and the first round's task holds none. The driver
# (packages/daemon/internal/daemon/review_range_live_test.go, which this script builds with
# `-tags e2e`) builds and starts the real Dispatch server from this checkout's packages/envoy on a
# database of its own, boots the whole daemon with dispatch_url at
# it, writes each role's handoff with the real `dispatch` CLI (packages/envoy-client/bin/dispatch.ts
# under bun, as each claim's registered session), completes each phase through the daemon's POST
# /legion/v1/handoff/complete, and publishes the review App's review, the implementer's push and
# GitHub's synchronize as Envoy relays GitHub's webhooks. Each check reads the task the daemon
# prompted the reviewer's shim with, and first holds that it is the reviewing task of its round:
# its `phase` field reads `reviewing`, and its handoff fields name the tester's handoff of that
# round by its version. Each
# check prints `== <name>`, what it observed and `ok <name>`; the first that fails ends the run
# non-zero, naming it:
#   - round-1-names-no-commit: an issue the tester passes reaches reviewing, and the reviewer's
#     task ends with the line `Last reviewed commit: none. Review entry: <n>.`;
#   - round-2-names-the-commit-of-round-1: the reviewer requests changes at the pull request's head
#     X, its footer naming the session its claim registered and the entry its task ended with
#     (`Review entry: <n>.`, read from the task the daemon delivered, as an agent reads it); the
#     implementer pushes a new head and completes at it, and the tester passes; the reviewer's task
#     holds `Last reviewed commit: X.`;
#   - control-no-record-no-commit: on a second issue, after the same first round and review, the
#     driver reads that phases.reviewed_head of the reviewer's row holds the head the review read,
#     clears it in the daemon's database, and the reviewer's task of round 2 ends with
#     `Last reviewed commit: none.` and its entry. The check before it is the same run with the
#     clear left out, so the control can fail;
#   - other-session-records-no-commit: on a third issue, after the same first round, the review
#     App's request for changes at the head comes from a session the reviewer's claim did not
#     register, naming the entry the reviewer's task named; the daemon sets it aside, so the issue
#     is still in reviewing, and phases.reviewed_head of the reviewer's row stays empty. The second
#     check is the same first round with the reviewer's own session, which moves the issue and
#     records the head.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/review-range.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. REVIEW_RANGE_EVIDENCE_DIR (default a fresh directory under TMPDIR,
# or /tmp when it is unset, kept and printed) keeps the transcript, the nats-server's log, the
# daemon's, Dispatch's (dispatch.log) and the driver's output. Nothing reaches github.com or a
# deployed Dispatch. The run around the driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
command -v bun >/dev/null || {
  echo "FAIL prerequisites: bun is not on PATH; the dispatch CLI the proof drives runs under it" >&2
  exit 1
}
[ -d "$root/node_modules" ] || {
  echo "FAIL prerequisites: $root/node_modules is missing; run \`bun install\` at the checkout's root first" >&2
  exit 1
}
# The driver binary runs from this directory, not the package's, so it is told the checkout it
# builds Dispatch from and runs the CLI out of.
export LEGION_E2E_REPO_ROOT="$root"
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'review range' review-range TestReviewRangeLive \
  "${REVIEW_RANGE_EVIDENCE_DIR:-}"
