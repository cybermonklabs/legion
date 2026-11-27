-- 20261123072431_task_copy_remove.up.sql — the outbox kind that removes the working copy of a task
-- that a later version of the plan removed (task_copy_remove, record.TaskCopyRemove).
--
-- A later version that removes a waiting or stopped task deletes its rows of the plan; the task's
-- working copy, the jj workspace on its branch beside the issue's own working copy, stays on the
-- daemon's host until a row of this kind, queued in the version's transaction, removes it. The
-- migration touches no table but the outbox's check.
--
-- The outbox check is one list, so this migration names every kind: the thirty-five kinds of
-- 20261122122826, main's two basis seeds (pending_basis_seed and reparent_seed, 20261122154643) and
-- its merge_holds_changed (20261126110259) among them, and task_copy_remove, thirty-six in all. Its
-- version stands above 20261122122826 on purpose, and above main's 20261122154643, which writes the
-- list without this branch's task kinds: Migrate applies every unrecorded migration in version
-- order, so the list with the higher version is the last word on any database, and a list of this
-- version without the two seeds would refuse a queued row of either, and the upgrade with it
-- (SQLSTATE 23514). It stands below main's 20261126110259, so on a database main migrated it runs
-- late, after that one, and is the last word there: a list of this version without
-- merge_holds_changed would refuse a queued row of that kind in the same way.
--
-- There is no down file. This directory keeps none, so the way back is the transaction that ends
-- this header, its last paragraph (migrations.WayBack), run with the daemon stopped, before the
-- earlier image is pinned, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1, before the way back
-- of any earlier migration. The release before decodes no task_copy_remove row, so the block
-- deletes each such row first, since the check written again without the kind refuses a row that
-- holds it; then writes the check again without task_copy_remove, keeping the two seeds and
-- merge_holds_changed, as this file's own list does, since main's 20261122154643 and 20261126110259
-- stay recorded and a queued row of any of the three kinds stays with them, which a check without
-- its kind would refuse, and the way back with it (SQLSTATE 23514); and deletes its version
-- row, which is owed to the roll-forward, so that it runs this file again. What the way back costs
-- is the removal of each working copy such a row named: the copy stays on the host until the
-- issue's own workspace is removed.
--
-- begin;
-- delete from outbox where kind = 'task_copy_remove';
-- alter table outbox drop constraint if exists outbox_kind_check;
-- alter table outbox add constraint outbox_kind_check check (kind in (
--   'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
--   'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
--   'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
--   'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
--   'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
--   'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
--   'merge_holds_changed', 'task_branch', 'task_pull_requests_close'));
-- delete from schema_version where version = 20261123072431;
-- commit;
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'task_branch', 'task_pull_requests_close', 'task_copy_remove'));
