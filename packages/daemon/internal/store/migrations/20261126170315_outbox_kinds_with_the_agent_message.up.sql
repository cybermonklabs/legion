-- 20261126170315_outbox_kinds_with_the_agent_message.up.sql — the outbox's kind check with the
-- answer loop's agent_message beside every kind main's lists admit.
--
-- Three migrations write the check whole, each with a kind of its own: main's
-- 20261122154643_outbox_kinds_with_the_basis_seeds (dispatch://INFRA-3320) adds pending_basis_seed
-- and reparent_seed to the design news's list, main's 20261126110259_merge_holds_changed adds
-- merge_holds_changed to that one, and this branch's 20261124114223_released_agent_messages
-- (dispatch://INFRA-204) adds agent_message. Migrate applies every unrecorded migration in version
-- order, so the list of the highest version is the check a database stands on, and a list without
-- one of those kinds refuses every row of it at the outbox insert (SQLSTATE 23514). main's
-- 20261126110259 stands above 20261124114223 and leaves agent_message out, so the list that admits
-- every kind must stand above both, which this file does. 20261124114223 names every kind of
-- main's lists too, since a database current main migrated can hold a queued row of each of them
-- and Migrate applies 20261124114223 there, below that database's newest, before this file.
--
-- This list names every kind the outbox's lanes run (record.LaneKinds): the thirty kinds of the
-- design news's list (20261119203047), pending_basis_seed, reparent_seed, merge_holds_changed and
-- agent_message. It drops the constraint if it exists before adding it, so it is safe on a database
-- at either branch.
--
-- This migration was numbered 20261126132542 from this branch's merge of main at 26d8c86dd614 until
-- its merge of a2ac5fd7aceb brought in 20261126110259, which stands below that version and would
-- be applied after it on a database that recorded it, leaving agent_message out of the check for
-- good. No deployment ran it. Migrate passes over a recorded version that names no file, so a
-- database a build of this branch migrated applies this file again at the upgrade. Such a database
-- lacks main's 20261126110259, below its newest, and that list leaves agent_message out, so a
-- queued agent_message row stops its next upgrade there (SQLSTATE 23514), as one does at main's
-- 20261122154643 on a database a build of this branch migrated before its merge of 26d8c86dd614. On
-- such a database, with every daemon of it stopped, an operator first deletes those rows
-- (`delete from public.outbox where kind = 'agent_message';`), then runs the upgrade, which ends
-- with this file and admits the kind again. The delete costs each of those messages: the released
-- agent it was for is not woken for it. No release carries 20261124114223 or this file, so no
-- deployment's database is such a database.
--
-- A rollback below the release that answers for a stopped agent leaves the check as it is: it
-- admits every kind an earlier release writes, and no earlier release writes agent_message
-- (docs/kubernetes.md, "Rolling back below the release that answers for a stopped agent").
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'agent_message'));
