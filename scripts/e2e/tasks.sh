#!/usr/bin/env bash
# The live proof that an issue's plan of several tasks runs each task once the tasks it needs are
# done: the owner gives Legion a plan whose tasks need each other with register_plan, and the
# whole daemon runs two tasks at one time where neither needs the other, holds a step whose agent
# works a step of another task until that agent is free, takes a later version of the plan by the
# state of each task, and refuses a plan whose needs form a cycle. The driver
# (packages/daemon/internal/daemon/tasks_live_test.go, which this script builds with `-tags e2e`)
# builds and starts the real Dispatch server from this checkout's packages/envoy on a database of
# its own, boots the whole daemon with dispatch_url at it over four issues of one project, gives
# each plan.md through Dispatch and register_plan through the daemon's API as a claim does, plays
# each agent of each step over the fake runtime, and runs the real `dispatch` CLI
# (packages/envoy-client/bin/dispatch.ts under bun) as a person of the project. GitHub is a fake
# API over a real bare repository, so each task's branch, join and pull request is one git can
# read; a person's review and the merge are envelopes on the run's broker. This release runs one
# agent of each role for an issue, so the steps of one role share one key in every task. Each
# check prints `== <name>`, what it observed and one line `ok <name>`, `FAIL <name>`, or
# `UNKNOWN <name>: <why>` when it read nothing to judge; the first that does not pass ends the run
# non-zero, naming it:
#   - tasks-at-one-time: the example plan, a and b needing nothing, c needing b, d needing a and
#     c, e needing d; once the gate opens a and b both run before either is done while c, d and e
#     wait; c starts once b is done, d once a and c are, e once d is, and e's pull request merges;
#   - steps-read: `dispatch steps --issue <KEY>` as the person, once that issue is done, prints its
#     five tasks, then each task of the plan with its needs and each planned step passed, as
#     `task d needs [a,c]: 1 implementer:i passed, 2 reviewer:r passed`;
#   - step-waits-for-agent: tasks x and y whose first steps are of the one key k; once the gate
#     opens x's step runs and y's waits for k; once x's step ends y's starts in k's one session,
#     and the implementer was launched once;
#   - later-version: while task a runs, version 2 adds f, changes the waiting g and removes the
#     waiting k: f runs, g waits with its new steps, k is gone and never started; version 3, which
#     changes the running a, is refused 409 PLAN_REFUSED with the one reason `task a is running:
#     stop it with stop_task before you change it.`;
#   - cycle-refused: a plan whose a needs b and b needs a is refused 409 PLAN_REFUSED with the
#     reason `task a needs itself: a needs b, b needs a.`, and records nothing.
# TASKS_CONTROL names one control, a change of the driver's input its check must fail on; a run
# with a control never passes:
#   c-needs-nothing    c needs nothing and d needs a, b and c: tasks-at-one-time fails on c, which
#                      runs at the gate beside a and b;
#   rows-deleted       the example issue's plan rows deleted once its plan is given:
#                      tasks-at-one-time reads no task and ends UNKNOWN;
#   node-task-deleted  task e deleted from the issue's plan node in Dispatch's store: steps-read
#                      fails on the line of task e;
#   y-own-agent        y's first step a planner of its own: step-waits-for-agent fails on y's
#                      step, which starts at the gate beside x's;
#   v3-changes-g       version 3 changes only the waiting g: later-version fails on its answer,
#                      200;
#   cycle-cut          a needs nothing: cycle-refused fails on its answer, 200.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     [TASKS_CONTROL=<control>] bash scripts/e2e/tasks.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. TASKS_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp
# when it is unset, kept and printed) keeps the transcript, the nats-server's log, the daemon's,
# Dispatch's (dispatch.log) and the driver's output. Nothing reaches github.com or a deployed
# Dispatch. The run around the driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
require_dispatch_cli
live_daemon_proof 'tasks' tasks TestTasksLive "${TASKS_EVIDENCE_DIR:-}"
