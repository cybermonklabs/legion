-- 20261114080300_outbox_kinds_with_the_wait_due.up.sql — the outbox kind that brings a phase
-- worker's wait to its time (wait_due, dispatch://INFRA-2040 R3, D5 of its plan).
--
-- A worker's declaration of a wait queues one row due at the wait's time, in the transaction that
-- records the wait (record.WaitDue, workflow's waitDeclared), as a production check's next look is
-- a check_wake row. The lifecycle lane runs it when it comes due, under every runtime, and its
-- handler ends the wait and writes the wait's own task, or ends nothing for a wait that is already
-- over (workflow's waitDue).
--
-- The check is one list, so the migration that sets it names every kind, as 20261106041346,
-- 20261108000545, 20261110235320 and 20261113082214 did: whichever of two open branches merges last
-- admits both their kinds. The list is 20261113082214's twenty-two kinds with wait_due added. It
-- drops the constraint if it exists before adding it, so it is safe on a database at either branch.
--
-- A rollback to a release before this one needs nothing of these rows: that release puts the kind
-- in no outbox lane (record's laneKinds), so it never decodes or runs a wait_due row, and the row
-- holds back no other row. The roll forward runs each row that came due meanwhile at once.
--
-- There is no down file: migrations here are forward-only.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'wait_due'));
