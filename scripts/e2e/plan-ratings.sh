#!/usr/bin/env bash
# The live proof that each retro of an issue rates the plan the issue ran, and that a person lists
# the finished issues of a project, each with its plan, its ratings with their reasons, its time and
# its count of failed steps, with `dispatch plans`. The driver
# (packages/daemon/internal/daemon/plan_ratings_live_test.go, which this script builds with
# `-tags e2e`) builds and starts the real Dispatch server from this checkout's packages/envoy on a
# database of its own, boots the whole daemon with dispatch_url at it over four issues of one
# project, and reads with the real `dispatch` CLI (packages/envoy-client/bin/dispatch.ts under bun)
# as a person of the project, with the personal token Dispatch minted for that person. Each check
# prints `== <name>`, what it observed and `ok <name>`; the first that fails ends the run non-zero,
# naming it:
#   - three-finished-issues-each-rated: three issues walk planning to done, the first with one
#     failed review, and their retros rate the plan 3, 4 and 5, each with a reason; `dispatch plans
#     --project <PROJECT>` prints the three, newest close first, each with the default plan, its one
#     rating with its reason, its time and `failed steps` 1, 0 and 0;
#     scripts/e2e/plan-ratings-read.sh, which runs that command itself against the same Dispatch,
#     with a `dispatch` on PATH that runs the same CLI, passes on the first issue and fails on the
#     fourth, which is not finished; control: the failed review's record deleted from Dispatch's
#     database (its node and its tuples), the same parse reports the first issue's `failed steps 0`
#     where 1 is wanted;
#   - a-retro-with-no-rating-is-refused: the fourth issue walks to its retro, and the retro's
#     completion with no rating, from a claim at the contract that added the rating, is refused
#     400 PLAN_RATING_REQUIRED, with the issue still in retro; control: the same completion with
#     planRating 4 and a reason moves the issue to merging. The fourth issue goes no further, so
#     the read by project lists the three alone.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/plan-ratings.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. PLAN_RATINGS_EVIDENCE_DIR (default a fresh directory under TMPDIR,
# or /tmp when it is unset, kept and printed) keeps the transcript, the nats-server's log, the
# daemon's, Dispatch's (dispatch.log) and the driver's output. Nothing reaches github.com or a
# deployed Dispatch. A run takes about a minute with a warm Go build cache and up to five minutes
# cold, most of it building Dispatch and the driver.
# The run around the driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
require_dispatch_cli
live_daemon_proof 'plan ratings' plan-ratings TestPlanRatingsLive "${PLAN_RATINGS_EVIDENCE_DIR:-}"
