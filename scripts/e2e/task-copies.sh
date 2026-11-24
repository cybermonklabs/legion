#!/usr/bin/env bash
# The live proof that each task of an issue's plan works in a working copy of its own, from the
# join of the tasks it needs, in the issue's one pod: the owner gives Legion a plan of several tasks
# with register_plan, and the whole daemon gives each task its own branch and working copy, has two
# agents work at one time in two directories, starts a task that needs others from the join of
# their work, tells the first step of a task whose needs conflict to repair that join on its join
# branch, and makes every launch and every adoption of a working copy of an issue in that issue's
# one pod. The driver (packages/daemon/internal/daemon/task_copies_live_test.go, which this script
# builds with `-tags e2e`) builds and starts the real Dispatch server from this checkout's
# packages/envoy on a database of its own, boots the whole daemon with dispatch_url at it over two
# issues of one project, gives each plan.md through Dispatch and register_plan through the daemon's
# API as a claim does, and plays each agent of each step over the fake runtime. Each adoption of a
# working copy the daemon asks of that runtime makes the working copy for real, as the shim of the
# issue's pod makes it (a jj workspace of one clone, in the directory the step's task names), and
# each agent works there with the real git and jj. GitHub is a fake API over a real bare repository,
# so each task's branch, the join of its needs and each pull request is one git can read; a
# person's review and the merge are envelopes on the run's broker. This release runs one agent of
# each role for an issue, so the steps of one role share one key in every task, and the two tasks
# that run at one time start at steps of two roles. Each check prints `== <name>`, what it observed
# and one line `ok <name>`, `FAIL <name>`, or `UNKNOWN <name>: <why>` when it read nothing to
# judge; the first that does not pass ends the run non-zero, naming it:
#   - two-tasks-two-copies: the example plan, a and b needing nothing, c needing b, d needing a and
#     c, e needing d; once the gate opens, b's planner is in its turn in the working copy of
#     legion/<KEY>.b while a's implementer is given its task and starts its turn in the issue's own,
#     legion/<KEY>: two turns at one time in two directories, each the one the adoption of its
#     branch made;
#   - d-starts-from-the-join: the walk to e's merge; the last commit of a, of b and of c are each an
#     ancestor of the commit d's working copy started at, and the last commit of each other task of
#     the commit e's started at;
#   - join-conflict-repaired: on a second issue whose a and c each change line 1 of x.txt, which
#     main holds, d's first step is told GitHub could not join c into its join branch
#     legion/<KEY>.d.join; the agent runs the jj commands its task tells in its working copy and
#     pushes its own work; the join branch then holds a's and c's last commits and x.txt repaired
#     with no conflict marker, d's own work starts from it, and d's pull request against it lists
#     d.txt alone;
#   - one-pod: every launch and every adoption of each issue is in the pod the Sandbox runtime gives
#     that issue, and the agent of each step was launched once.
# TASK_COPIES_CONTROL names one control, a change of the driver's input its check must fail on; a
# run with a control never passes:
#   rows-deleted           the example issue's plan rows deleted once its plan is given:
#                          two-tasks-two-copies reads no task and ends UNKNOWN;
#   adoption-drops-branch  every adoption reaches the shim naming no branch, so it makes the
#                          issue's own working copy alone: two-tasks-two-copies fails on b's step,
#                          told a directory no adoption made;
#   no-needs               c needs nothing, and e needs b and d: d-starts-from-the-join fails on
#                          b's last commit, which d's start lacks;
#   starts-unrecorded      the commit d's working copy started at forgotten before the read:
#                          d-starts-from-the-join reads no start and ends UNKNOWN;
#   no-repair              the agent of d's first step acts on a task that tells it no repair:
#                          join-conflict-repaired fails on c's last commit, which the join branch
#                          lacks;
#   lasts-unrecorded       the last commits of the second issue's a and c forgotten before the
#                          read: join-conflict-repaired reads no need's commit and ends UNKNOWN;
#   implementer-gone       the runtime reports the second issue's implementer gone and the daemon
#                          launches it again: one-pod fails on its second launch;
#   calls-unread           the check reads no call of the runtime: one-pod reads no launch and
#                          ends UNKNOWN.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them, and each working copy is a jj workspace), bun (after `bun install` at the
# checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     [TASK_COPIES_CONTROL=<control>] bash scripts/e2e/task-copies.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. TASK_COPIES_EVIDENCE_DIR (default a fresh directory under TMPDIR,
# or /tmp when it is unset, kept and printed) keeps the transcript, the nats-server's log, the
# daemon's, Dispatch's (dispatch.log) and the driver's output. Nothing reaches github.com or a
# deployed Dispatch. The run around the driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
require_dispatch_cli
live_daemon_proof 'task copies' task-copies TestTaskCopiesLive "${TASK_COPIES_EVIDENCE_DIR:-}"
