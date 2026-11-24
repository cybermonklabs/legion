#!/usr/bin/env bash
# The live proof that the owner of an issue does a step of its plan, and that a task with no pull
# request ends with no merge: a step whose key is owner, and the retro of a task whose retro is the
# owner's, are told to the owner, the issue's own architect, as one notice owner-step and start no
# claim; the owner reports each with complete_step (POST /legion/v1/steps/complete); and a task
# whose issue has no pull request is done when its retro ends. The driver
# (packages/daemon/internal/daemon/owner_does_step_live_test.go, which this script builds with
# `-tags e2e`) builds and starts the real Dispatch server from this checkout's packages/envoy on a
# database of its own, boots the whole daemon with dispatch_url at it over three issues of one
# project, drives the real `dispatch` CLI (packages/envoy-client/bin/dispatch.ts under bun) as the
# owner's session and as a person of the project, and the daemon's API as each claim. The owner
# writes each plan.md with `dispatch artifact` and gives it with register_plan before the issue's
# gate opens. GitHub is a fake API the daemon reads on its own, which lists the first issue's pull
# request from the owner's report of its step on; the owner's notices are read where Envoy's
# listener takes them. Each check prints `== <name>`, what it observed and one line `ok <name>` or
# `FAIL <name>`; the first that fails ends the run non-zero, naming it. Each control prints the
# check it runs as `control: FAIL <check> over <KEY>`, with what that check says:
#   - owner-step: the first issue is given `implementer:owner`; its owner is told one owner-step of
#     the implementer's step in implementing, whose task names step 1 and the text of the task's
#     block, and no claim of the issue is launched for the step, neither a fresh implementer nor the
#     owner's own claim relaunched; with the pull request open on the issue's branch, the owner's
#     complete_step of step 1 with its commit and its implement handoff moves the issue to the
#     retro, whose owner-step the owner is told the same way; its complete_step of the retro (step
#     0) with a rating moves the issue to merging, and the merger is launched;
#   - control-owner-step: the third issue is given `implementer`, and the check of owner-step over
#     it fails: the daemon launches a fresh implementer, and the owner is told no owner-step;
#   - tester-alone: the second issue is given `tester`; the tester is prompted with its step and
#     the text of its task's block, and its pass moves the issue to the retro, whose owner-step the
#     owner is told with no claim launched for it. The check of the task's end fails there, the issue
#     standing in retro (a control); the owner's complete_step of the retro with a rating ends the
#     issue at done, no merger is launched, `dispatch steps --issue <KEY>` as the person prints step
#     1 tester and the retro of the owner and nothing else, the retro's record names the rating, and
#     `dispatch plans --project <PROJECT>` lists the issue with the owner's plan `tester` and that
#     rating;
#   - control-tester-alone: the same check over the first issue, whose task has a pull request,
#     fails on the launch of its merger.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/owner-does-step.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. OWNER_DOES_STEP_EVIDENCE_DIR (default a fresh directory under
# TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript, the nats-server's log,
# the daemon's, Dispatch's (dispatch.log) and the driver's output. Nothing reaches github.com or a
# deployed Dispatch. A run takes two to five minutes, most of it building Dispatch and the driver.
# The run around the driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
require_dispatch_cli
live_daemon_proof 'owner does step' owner-does-step TestOwnerDoesStepLive \
  "${OWNER_DOES_STEP_EVIDENCE_DIR:-}"
