#!/usr/bin/env bash
# The live proof that when an issue Legion started closes and its last run had no retro, Legion
# runs that retro after the close, in the way it runs a production check after a merge, and that
# the retro of an open issue whose pull request merged sends its findings to the root architect
# the same way (INFRA-5602, A1 and A2).
#
# The proof runs twenty-one steps against one booted Go daemon (`legion start`'s `run`) over a real
# PostgreSQL and a native `nats-server`, with `liveDispatch` (the whole-daemon proofs' Dispatch
# stand-in), `noticeListener` and, for the findings of steps 11 and 12, the real Envoy listener
# (`cmd/listener`) running beside it on the proof's broker. The daemon boots with four roots: T,
# whose children the steps close, and R2, R3 and R4, which steps 12, 13 and 23 close. The steps
# run in this order, each with its number (the plan has no steps 15 and 16; steps 19 to 23 are the
# tester's live probes of the fixes of review rounds 1 to 4):
#   1. grandchild-closed: C's architect closes started grandchild G -> one retro task to G's
#      retro agent holding every R2 clause and naming T's architect (not C's), completed with a
#      plan rating; G stays done.
#   2. child-closed: T's architect closes started child C (whose implementer pushed a head) ->
#      one retro task with R2 clauses naming T's architect (not C's own), backward move refused
#      BACKWARD_REFUSED, completed with a plan rating; C stays done.
#   3. person-closed: a second started child closed by a person's done -> one retro task, left
#      open for step 10.
#   4. backlog-then-closed: a third child run into implementing, parked in backlog (no retro task),
#      then closed done -> implementer resumed in its kept session, retro completes.
#   5. retro-had: a fourth child completes its retro and closes by merge -> no retro after close.
#   6. never-started: a fifth child closed before its first phase -> none.
#   7. parent-closed: parent P held short of its first phase (blocked_by an open issue) while its
#      child PC starts, closed done -> one retro task to P's implementer (Spawn), retro completes.
#   8. closed-in-retro: a sixth child closed done while its retro turn runs -> completion taken
#      200 with rating, claim and session kept with no suspend/resume/spawn, no second retro task.
#   9. merged-early: a seventh child whose PR merges before merging -> open retro task names T's
#      architect and the pending-sibling clause, completes, then merges closed with no second retro.
#  17. sent-back-then-closed: another child whose retro moves work back to implementing (row
#      ends failed, no rating), then closed done -> no retro after close.
#  18. retro-task-held-at-close: another child signed off into its retro while its implementer
#      runs a turn of its own, then closed done -> the retro's task the claim held reaches the
#      agent once its turn ends, completes with rating, no other retro task.
#  14. todo-ends-retro: another child closed (retro row starts), set back to todo (row ends
#      stopped, no rating), then closed again before run 2's first phase -> no retro after close.
#  19. todo-drops-held-task: another child closed done while its implementer runs a turn of its
#      own (the retro's task waits on the claim), then set back to todo (row ends stopped) -> once
#      its turn ends the agent gets no retro task.
#  20. member-left-open: another child closed done -> one retro task, its retro left open past
#      T's close for step 21.
#  11. root-closed: once all children close, T starts its own first phase and a person closes T ->
#      one retro task to T's retro agent; a finding published through the real Envoy listener wakes
#      and resumes T's suspended architect in its kept session and is delivered to it; T's retro
#      completes.
#  10. member-open-at-root-close: step 3's open retro survived T's close and completes with rating.
#  21. todo-ends-lingering-trees-last-retro: T's woken architect ends its turn, and step 20's
#      child, the last open retro of T's lingering tree, is set back to todo (row ends stopped) ->
#      T's architect suspended, and its stop for the hour after queued (linger_close retrosEnded).
#  12. root-closed-in-linger: root R2 run into implementing, parked in icebox, closed done inside
#      its default linger -> implementer resumed in its kept session, retro completes; R2's
#      architect woken for the finding.
#  22. woken-root-stops: R2's woken architect ends its turn, the hour after R2's last retro made to
#      pass and its linger_close retrosEnded row brought due -> R2's architect suspended.
#  13. expired-root-closed: root R3 run into implementing, parked in triage, its linger end brought
#      forward so its claims retire, then closed done -> implementer relaunched in its kept session
#      in a reopened tree lifecycle, retro completes.
#  23. past-linger-todo-reserves-cleanup: root R4 with a started child closed done (retro left
#      open), R4 closed done and its retro completed, R4's linger end brought forward, then the
#      child set back to todo (row ends stopped) and the hour after made to pass -> R4's tree
#      cleanup reserved.
#
# Each step runs on its own, printing `== <step>` and `ok <step>` or `FAIL <step> at <assertion>`,
# so a step that fails does not stop the next; the run exits non-zero if any step failed.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later, awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/retro-after-close.sh
# LEGION_TEST_PG_DSN is required and names a loopback PostgreSQL server; prerequisites refuses any
# non-loopback host with exit 2 before the driver builds or the broker starts. The broker is the
# run's own: LEGION_E2E_NATS_BIN, or the pinned v2.15.1 installed with `go install`, started with
# JetStream on a port the kernel picks and stopped on any exit. RETRO_AFTER_CLOSE_EVIDENCE_DIR
# (default a fresh directory under TMPDIR, or /tmp when unset, kept and printed) holds the
# transcript, the nats-server's log, the daemon's own log and the driver's output.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'retro after close' retro-after-close TestTheRetroRunsAfterTheCloseLive \
  "${RETRO_AFTER_CLOSE_EVIDENCE_DIR:-}"
