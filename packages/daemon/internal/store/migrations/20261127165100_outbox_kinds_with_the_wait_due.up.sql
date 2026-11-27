-- 20261127165100_outbox_kinds_with_the_wait_due.up.sql — the outbox kind that brings a phase
-- worker's wait to its time (wait_due, dispatch://INFRA-2040 R3, D5 of its plan).
--
-- A worker's declaration of a wait queues one row due at the wait's time, in the transaction that
-- records the wait (record.WaitDue, workflow's waitDeclared), as a production check's next look is
-- a check_wake row. The lifecycle lane runs it when it comes due, under every runtime, and its
-- handler ends the wait and writes the wait's own task, or ends nothing for a wait that is already
-- over (workflow's waitDue).
--
-- The check is one list, so the migration that sets it names every kind. This one follows the list
-- of 20261126163320_state_events: that migration's thirty-six kinds — step_record, plan_record,
-- conflict_confirm, merge_forward, design_changes, pending_basis_seed, reparent_seed,
-- merge_holds_changed, state_change, state_admission, state_boot and phase_ended among them — and
-- this one, thirty-seven in all. Its version stands above every migration of main, and above
-- 20261126163320 on purpose: Migrate applies every unrecorded migration in version order, so the
-- list with the higher version is the last word on any database. A version below 20261126163320
-- would let that migration run after this one on a fresh database and write its list without
-- wait_due, which would refuse every wait_due row, and a list that left out any of 20261126163320's
-- kinds would refuse every row of it and fail the upgrade of a database that holds one pending. It
-- drops the constraint if it exists before adding it, so it is safe on a database at either
-- release.
--
-- A rollback to a release before this one needs nothing of these rows: that release puts the kind
-- in no outbox lane (record's laneKinds), so it never decodes or runs a wait_due row, and the row
-- holds back no other row. The name stays in the check, which refuses nothing an earlier release
-- writes. The roll forward runs each row that came due meanwhile at once.
--
-- There is no down file: migrations here are forward-only.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'state_change', 'state_admission', 'state_boot', 'wait_due'));
