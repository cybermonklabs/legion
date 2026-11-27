-- 20261126194217_outbox_kinds_of_the_task_copies.up.sql — the outbox kind list of the merge holds
-- change (20261126110259) with the three kinds of each task's working copy added.
--
-- Three migrations of this release add a kind to the outbox kind check: 20261121165047 adds
-- task_branch, 20261122122826 adds task_pull_requests_close, and 20261123072431 adds
-- task_copy_remove. Two migrations of main stand between and above them and write the whole list
-- again without those three kinds: 20261122154643 (the basis seeds) and 20261126110259 (the merge
-- holds change). On a fresh database Migrate applies every migration in version order, so the list
-- of 20261126110259 would be the last word and refuse each of the three kinds. This migration names
-- every kind, as 20261126110259 does: its thirty-three kinds, in its order, and then the three,
-- thirty-six in all. Its version stands above 20261126110259, so on any database this list is the
-- last word.
--
-- On a database main's release migrated, this release's three earlier migrations are applied over
-- rows of kinds main's daemon queues, pending_basis_seed, reparent_seed and merge_holds_changed
-- among them. A list without such a kind would fail its ADD CONSTRAINT on that row (SQLSTATE
-- 23514), and Migrate with it (store's TestADatabaseMainMigratedWithRowsOfItsNewestKindsUpgrades).
-- So none of the three writes a list of its own: each adds its kind to the list the database holds,
-- which keeps every kind of main's, and on a fresh database a kind main adds is still refused below
-- the migration of main's that adds it. This list, the one this release writes whole, names all
-- thirty-six kinds.
--
-- A rollback to the release before runs no statement of this migration: the ways back of
-- 20261123072431, 20261122122826 and 20261121165047 delete the rows of their own kinds and take
-- each kind out of the list the database holds, and this list takes every kind the release before
-- queues. The way back is:
--
-- delete from schema_version where version = 20261126194217;
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'task_branch', 'task_pull_requests_close', 'task_copy_remove'));
