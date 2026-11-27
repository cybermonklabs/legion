-- 20261126144204_checks_of_the_merge_with_main.up.sql — the phase check and the outbox kind check
-- written again with every value of both branches that merged: this branch's phase step and its
-- three task kinds (task_branch, task_pull_requests_close, task_copy_remove), and main's phase
-- awaiting_children, its two basis seeds (pending_basis_seed, reparent_seed;
-- dispatch://INFRA-3320) and its merge_holds_changed (main's 20261126110259).
--
-- The rule this file and every migration of this branch that writes a list follow: the last
-- migration to write a list owns every value of it, main's included. A list that leaves out a
-- value main added is the last word on a database main migrated, and a row of that kind is
-- refused there (SQLSTATE 23514), which stops the upgrade and the daemon's boot with it.
--
-- Each check is one list, and each branch wrote its own lists of it. Main's 20261114232555 names
-- awaiting_children without step, its 20261122154643 the two basis seeds without the three task
-- kinds, and its 20261126110259 merge_holds_changed without them. This branch's 20261117223524
-- names step and awaiting_children, which main's 20261114232555 wrote below it; its
-- 20261121165047 and 20261122122826 name task_branch and task_pull_requests_close with main's two
-- seeds and its merge_holds_changed, which they must, since they sort below main's 20261122154643
-- and 20261126110259 and so are the last word on a database main migrated; and its 20261123072431
-- names every task kind, both seeds and merge_holds_changed, for the same reason. This
-- file stands above all of them and above main's 20261126110259, so it is the last word on every
-- database, and it names every value of both branches: 36 kinds, main's merge_holds_changed among
-- them. On a fresh database Migrate applies every migration in version order and this file ends
-- the lists; on a database main migrated, or one a build of this branch migrated before its merge
-- of main, Migrate applies the unrecorded migrations late and this file still ends them. Every
-- database ends with the same two lists.
--
-- An upgrade applies a list late over the rows the table holds, and a row the list refuses stops
-- it (SQLSTATE 23514) before this file runs. That is why 20261121165047, 20261122122826 and
-- 20261123072431, which sort below main's 20261126110259 and so run after it on a database main
-- migrated, name main's two basis seeds and its merge_holds_changed in their own lists: a queued row
-- of any of them stands, the upgrade runs through, and no operator deletes a row. On a database a
-- build of this branch migrated, main's 20261114232555 refuses an issue that stands at the phase
-- step (it leaves issues_held_from_check, which keeps step, as it was), and main's 20261122154643
-- and 20261126110259 each refuse a queued row of the three task kinds. No deployment holds such a
-- database: only a build of this branch made one. With every daemon of it stopped, an operator
-- first finishes or stops each task whose issue stands at step, and deletes the queued rows of the
-- three kinds (`delete from outbox where kind in ('task_branch', 'task_pull_requests_close',
-- 'task_copy_remove')`), then runs the upgrade. What that delete costs is the work those rows owed:
-- a task's branch made on GitHub, the close of the other pull requests of an issue, and the removal
-- of a removed task's working copy.
--
-- There is no down file. This directory keeps none, so the way back is the transaction that ends
-- this header, its last paragraph (migrations.WayBack), run with the daemon stopped, before the
-- earlier image is pinned, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1, before the way back
-- of any earlier migration. The lists this file writes are the ones 20261117223524 and
-- 20261123072431 write, so the block writes no check and deletes no row of any table but
-- schema_version: it deletes this file's version row, which is owed to the roll-forward, so that
-- it runs this file again. The way backs of those earlier migrations write each list as the
-- release before them wrote it.
--
-- begin;
-- delete from schema_version where version = 20261126144204;
-- commit;
alter table issues drop constraint issues_phase_check;
alter table issues add constraint issues_phase_check check (phase in (
  'admitted', 'awaiting_person', 'awaiting_children', 'planning', 'implementing', 'testing',
  'reviewing', 'step', 'accepting', 'retro', 'merging', 'done', 'held'));

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'task_branch', 'task_pull_requests_close', 'task_copy_remove'));
