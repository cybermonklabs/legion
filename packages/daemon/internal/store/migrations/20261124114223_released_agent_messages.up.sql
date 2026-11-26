-- 20261124114223_released_agent_messages.up.sql — the answer loop's messages for an agent whose
-- claim the daemon released.
--
-- The answer loop answers for an agent whose claim the daemon released (suspended_by = 'released':
-- an idle release of its issue pod, or a project's pause) as it answers for any stopped agent, and
-- gives that agent everything else it does not answer through one outbox row that wakes the claim
-- and delivers the message once the woken session holds its role (dispatch://INFRA-204). That row
-- is the outbox kind agent_message (record.AgentMessage). The outbox's kind check is one list, so
-- it is written again whole, naming every kind the design news's 20261119203047 names and the two
-- basis seeds main's 20261122154643 adds (pending_basis_seed and reparent_seed), with
-- agent_message added. A database current main migrated can hold a queued row of either seed, and
-- Migrate applies this version there, below that database's newest, before
-- 20261126132542_outbox_kinds_with_the_basis_seeds_and_agent_messages: `ADD CONSTRAINT … CHECK`
-- validates every row the table holds, so a list here without the seeds would fail that upgrade.
-- 20261126132542 writes the same list again, and its version, above this one's and main's, makes
-- it the last word of the check on any database Migrate takes.
--
-- answer_questions.wake marks a question whose claim was released when the loop received or
-- decided it: a person's decision that also asks, received for a released claim, is held for the
-- row the loop writes once the question is posted, and a reply the loop does not post for a
-- released claim is given to the agent through that row in its place. Every row before this one
-- was received and decided without it, so the column's default is false. Such a question ends
-- woken, a state the state check names beside the seven 20261106214002 wrote; the check is one
-- list, so this migration names every state.
--
-- A rollback below the release that answers for a stopped agent leaves the column and both checks
-- as they are: no earlier release reads or writes answer_questions, and none writes the kind
-- (docs/kubernetes.md, "Rolling back below the release that answers for a stopped agent").
alter table answer_questions add column wake boolean not null default false;
alter table answer_questions drop constraint if exists answer_questions_state_check;
alter table answer_questions add constraint answer_questions_state_check
  check (state in ('received', 'generating', 'appended', 'posted', 'forwarded', 'passed_on', 'dropped', 'woken'));

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'agent_message'));
