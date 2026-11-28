#!/usr/bin/env bash
# The live proof of the rollback (k44 of dispatch://INFRA-4730; R22 of dispatch://INFRA-3320): this
# release boots on a database the release before left, converts it, runs work of its own and stops;
# the step docs/kubernetes.md gives for a rollback ("Rolling back the release that stores no tree")
# runs; and the release before boots on the database as it then is and runs.
#
# The driver (packages/daemon/internal/daemon/tree_rollback_live_test.go, which this script builds
# with `-tags e2e`) builds and starts the real Dispatch server from this checkout's packages/envoy on
# a database of its own, with its events on the run's nats-server, its parent-status rule off, since
# production's cut-over and its rollback run before DISPATCH_PARENT_STATUS_RULE is turned on (k33),
# and its DISPATCH_LEGION_STATUS_SCOPE unset, so a session's status write of a Legion issue lands;
# makes the daemon's one database; and runs the release before's driver
# (tree_release_before_live_test.go, built from the release before's tree, lib/tree-proof.sh) as a
# child process twice. First that driver boots the release before and leaves live phases, a held
# issue, a tree that lingers with a hand-over row whose write landed and one whose write did not,
# an open production check, and K, a labelled child of Q, an issue nobody hands to Legion at
# in_progress, whose row is in its copy of Dispatch's issues when it stops. Then this release boots,
# converts,
# and, through a wrapper of its Dispatch client (overrides.wrapDispatch), answers every read of Q
# with a 503 and leaves Q out of every listing once its consumer has caught up, when the driver
# deletes Q's row from the copy; it moves an issue at work under another parent and releases it
# there (a claim made after the change), records a waiting issue and sets it under an unreleased
# child, unlinks a child, admits a new root whose first status write the wrapper refuses 403
# LEGION_STATUS_SCOPE_REQUIRED once (the status-scope notice) and whose gate the oldest plugin
# contract's legion tool registers, closes and reopens a root, and holds a session's backlog on K,
# whose chain the copy does not hold. It stops; the driver queues, in this release's shape, one
# effect of each kind the release before's own outbox_kind_check admits (read from the database that
# release migrated), a status write with an option and a reparent seed of R18's shape; and it runs
# the step: the SQL block through psql, and the relabel loop with a `kubectl` of its own first on
# PATH, which records each label the loop sets (the fake runtime makes no Sandbox). Where the
# release before does not carry INFRA-4728 (its embedded migrations hold no
# 20261114232555_awaiting_children), the rollback crosses that release as well, so the driver then
# runs INFRA-4728's step from the guide ("Rolling back the release that holds a parent's start
# while a child of it is open"), as an operator runs the steps of two releases in the guide's
# order: each of its SQL blocks, the clear of the given columns only where the release before does
# not carry 20261111225552_phase_entry, since the guide owes it only on a rollback below that. Where
# the release before carries INFRA-4728, this release's step runs alone. Then the release
# before boots again: the outbox holds no row its own decoder refuses; each claim acts; the
# architect of the root this release recorded registers its gate; the step printed K's held write
# and the release before writes K's reassert, so Dispatch shows K in_progress again while K works
# on; a new root and its claims start; each queued effect runs and none is refused; the issue whose
# parent changed stands in its old tree, as the unlinked one does; the issue whose parent changed
# after the conversion is held by that release's ancestry hold; the landed hand-over stands as a
# root of its own; and a root's close cleans up no tree that labels the pod of an issue at work. Each
# step prints `== <name>`, what it observed and `ok <name> (<s>s)`; the first that fails ends the
# run non-zero, naming it. Its last line is the driver's `tree rollback: PASS` and then the run's; a
# refusal before the run's checks, or a failure of a check of lib/tree-proof.sh's own, ends it on
# that FAIL line instead.
# What it cannot show: the `kubectl` relabel itself, which needs a cluster.
#
# Run it from a checkout on a Linux host with go, git, tar, psql, jj 0.38 or later (the daemon's
# boot refuses to start without it), bun (after `bun install` at the checkout's root), awk and
# python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/tree-rollback.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL both daemons migrate and run on; the
# driver also creates Dispatch's database and the daemon's on the same server (its role must be
# allowed to create and drop databases, and the server must offer the pgvector extension, which
# Dispatch's migrations create) and drops them on exit. That server must be this machine's:
# lib/live-daemon-proof.sh refuses a host that is not 127.0.0.1, localhost or ::1 with exit 2. The
# release before, read at run time (lib/tree-proof.sh), is `git merge-base HEAD origin/main` while
# that commit does not carry this release's conversion
# (packages/daemon/internal/store/migrations/20261122154645_issue_facts.up.sql), and once it does,
# the parent of the first-parent commit of origin/main that added that file, so the proof runs alike
# before and after this release reaches main: a shallow checkout, CI's, fetches its history first,
# and the oldest contract's commit with it. The
# broker is the run's own, a nats-server from LEGION_E2E_NATS_BIN or the pinned one installed with
# `go install`. TREE_ROLLBACK_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it
# is unset, kept and printed) keeps the transcript, the nats-server's log, both daemons' logs
# (daemon.log), Dispatch's (dispatch.log), the build of the release before's driver and the driver's
# output. Nothing reaches github.com or a deployed Dispatch; `bun install` of the old plugin root
# reaches the package registry the checkout's lockfile names, once per commit. A run takes five to
# fifteen minutes: two daemon builds, Dispatch's, and the old root's install on a cold cache.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
command -v psql >/dev/null || {
  echo "FAIL prerequisites: psql is not on PATH; the rollback step's two blocks run it" >&2
  exit 1
}
# shellcheck source-path=SCRIPTDIR source=lib/tree-proof.sh
. "$root/scripts/e2e/lib/tree-proof.sh"
tree_proof 'tree rollback' tree-rollback TestTheTreeRollbackLive "${TREE_ROLLBACK_EVIDENCE_DIR:-}"
