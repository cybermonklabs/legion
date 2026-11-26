-- 20261126132542_outbox_kinds_with_the_basis_seeds_and_agent_messages.up.sql — the outbox's kind
-- check with the design-gate basis's two seeds and the answer loop's agent_message together.
--
-- Two migrations write the check whole, each with a kind of its own: main's
-- 20261122154643_outbox_kinds_with_the_basis_seeds (dispatch://INFRA-3320) adds pending_basis_seed
-- and reparent_seed to the design news's list, and this branch's
-- 20261124114223_released_agent_messages (dispatch://INFRA-204) adds agent_message to it. Migrate
-- applies every unrecorded migration in version order, so a database that ran both stands on the
-- list of the higher, 20261124114223, and a database a build of this branch migrated before its
-- forward merge of main at 26d8c86dd614 recorded that version with the list it had then, which
-- left both seeds out. Migrate never runs a recorded version again, so on such a database the
-- boot's pending-basis seed of every step that waits on an ancestor's request, and every re-parent
-- seed, would be refused at the outbox insert (SQLSTATE 23514). The merge gave 20261124114223 both
-- seeds, for a database that has not recorded it yet: a database current main migrated can hold a
-- queued row of either, and Migrate applies 20261124114223 there, below that database's newest,
-- before this file, so a list there without them would fail that upgrade.
--
-- This list names every kind either migration names: the thirty kinds of the design news's list
-- (20261119203047), with pending_basis_seed, reparent_seed and agent_message, which are every kind
-- the outbox's lanes run (record.LaneKinds). Its version stands above both, so it is the last word
-- of the check on a fresh database, on one main migrated and on one this branch migrated. It drops
-- the constraint if it exists before adding it, so it is safe on a database at either branch.
--
-- A database a build of this branch migrated before that merge lacks main's 20261122154643, below
-- its newest, and that list leaves agent_message out, so a queued agent_message row stops its next
-- upgrade there (SQLSTATE 23514). On such a database, with every daemon of it stopped, an operator
-- first deletes those rows (`delete from public.outbox where kind = 'agent_message';`), then runs
-- the upgrade, which ends with this file and admits the kind again. The delete costs each of those
-- messages: the released agent it was for is not woken for it. No release carries 20261124114223
-- or this file, so no deployment's database is such a database.
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
  'agent_message'));
