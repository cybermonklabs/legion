-- 20261127142558_checks_of_the_merge_with_the_state_events.up.sql — the phase check and the outbox
-- kind check written again with every value of both branches that merged, above main's
-- 20261126163320_state_events: this branch's phase step and its three task kinds (task_branch,
-- task_pull_requests_close, task_copy_remove), and main's phase awaiting_children, its two basis
-- seeds (pending_basis_seed, reparent_seed; dispatch://INFRA-3320), its merge_holds_changed (main's
-- 20261126110259) and its three state kinds (state_change, state_admission, state_boot; main's
-- 20261126163320, dispatch://INFRA-3931).
--
-- The rule this file and every migration of this branch that writes a list follow: the last
-- migration to write a list owns every value of it, main's included. 20261126144204 wrote both
-- lists last until the merge of main at 17767e4917b1 brought main's 20261126163320, which stands
-- above it and writes the outbox kind check without the three task kinds: on a fresh database
-- Migrate applies 20261126163320 after 20261126144204, and the check then refuses every row of
-- those kinds the daemon queues. This file stands above 20261126163320 and every other migration,
-- so it is the last word on every database, and it names every value of both branches: 39 kinds
-- and 13 phases. On a fresh database Migrate applies every migration in version order and this
-- file ends the lists; on a database main migrated, or one a build of this branch migrated before
-- this merge of main, Migrate applies the unrecorded migrations late and this file still ends them.
-- Every database ends with the same two lists.
--
-- 20261126144204 keeps its version, since a build of this branch recorded it and a binary that no
-- longer carries a version the database recorded is refused. It names the three state kinds, as
-- 20261121165047, 20261122122826 and 20261123072431 do: on a database main migrated each of them
-- applies late, after 20261126163320, and a list without the state kinds would refuse a queued
-- state row (SQLSTATE 23514) and stop the upgrade before this file runs.
--
-- This file writes the phase check again too, though 20261126163320 does not write it: on a
-- database a build of this branch migrated before its merges of main, main's 20261114232555
-- applies late and writes the phase check without step, and the last migration to write a list is
-- its last word. On such a database main's 20261122154643, 20261126110259 and 20261126163320 each
-- refuse a queued row of the three task kinds, and 20261114232555 an issue that stands at step;
-- the header of 20261126144204 gives the statement an operator runs before the upgrade, with every
-- daemon of the database stopped, and what it costs. No deployment holds such a database: only a
-- build of this branch made one.
--
-- There is no down file. This directory keeps none, so the way back is the transaction that ends
-- this header, its last paragraph (migrations.WayBack), run with the daemon stopped, before the
-- earlier image is pinned, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1, before the way back
-- of any earlier migration. The lists this file writes are the ones 20261126144204 writes, so the
-- block writes no check and deletes no row of any table but schema_version: it deletes this file's
-- version row, which is owed to the roll-forward, so that it runs this file again. A rollback past
-- 20261126163320 too takes the steps of that release's own section of docs/kubernetes.md.
--
-- begin;
-- delete from schema_version where version = 20261127142558;
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
  'merge_holds_changed', 'task_branch', 'task_pull_requests_close', 'task_copy_remove',
  'state_change', 'state_admission', 'state_boot'));
