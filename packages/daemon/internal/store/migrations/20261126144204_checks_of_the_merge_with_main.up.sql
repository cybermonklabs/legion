-- 20261126144204_checks_of_the_merge_with_main.up.sql — the phase check and the outbox kind check
-- written again with every value of both branches that merged: this branch's phase step and its
-- three task kinds (task_branch, task_pull_requests_close, task_copy_remove), and main's phase
-- awaiting_children and its two basis seeds (pending_basis_seed, reparent_seed;
-- dispatch://INFRA-3320).
--
-- Each check is one list, and each branch wrote its own lists of it. Main's 20261114232555 names
-- awaiting_children without step, and its 20261122154643 the two basis seeds without the three
-- task kinds. This branch's 20261117223524 names step and awaiting_children, which main's
-- 20261114232555 wrote below it; its 20261121165047 and 20261122122826 name task_branch and
-- task_pull_requests_close and no seed, since they stand below main's 20261122154643, before the
-- seeds exist; and its 20261123072431 names every task kind and both seeds, since it stands above
-- that migration. On a fresh database Migrate applies every migration in version order, so
-- 20261117223524's phase list and 20261123072431's kind list are the last word, and this file
-- writes them again unchanged. On a database main migrated, Migrate applies this branch's
-- migrations late, in version order, and 20261123072431 and 20261117223524 again end with the
-- same lists. On a database a build of this branch migrated before its merge of main, Migrate
-- applies main's two late, after this branch's lists, and each takes this branch's values away;
-- this file stands above both, and above every other migration, so it puts them back. Every
-- database ends with the same two lists.
--
-- An upgrade applies a list late over the rows the table holds, and a row the list refuses stops
-- it (SQLSTATE 23514) before this file runs. On a database main migrated, this branch's
-- 20261121165047 and 20261122122826 refuse a queued row of either basis seed. With every daemon of
-- it stopped, an operator first deletes those rows (`delete from outbox where kind in
-- ('pending_basis_seed', 'reparent_seed')`), then runs the upgrade, as main's 20261122154643 says
-- for the same rows: the next boot queues a pending-basis seed again for each step that still
-- waits, and a re-parented root and its new parent's architect are not told of that one move. On a
-- database a build of this branch migrated, main's 20261114232555 refuses an issue that stands at
-- the phase step (it leaves issues_held_from_check, which keeps step, as it was), and main's
-- 20261122154643 refuses a queued row of the three task kinds. No deployment holds such a
-- database: only a build of this branch made one. With every daemon of it stopped, an operator
-- first finishes or stops each task whose issue stands at step, and deletes the queued rows of the
-- three kinds (`delete from outbox where kind in ('task_branch', 'task_pull_requests_close',
-- 'task_copy_remove')`), then runs the upgrade. What that delete costs is the work those rows
-- owed: a task's branch made on GitHub, the close of the other pull requests of an issue, and the
-- removal of a removed task's working copy.
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
  'task_branch', 'task_pull_requests_close', 'task_copy_remove'));
