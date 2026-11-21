-- 20261121101500_decisions_step_gone.up.sql — the store holds the design-gate step `decisions`
-- nowhere, and its checks admit `criteria` and `spec` alone.
--
-- The step `decisions` is gone: the daemon of the release before this one refuses a deployment
-- that gates it, so it writes the step nowhere, and every read of it leaves out what an earlier
-- daemon wrote of it. Three things of that earlier daemon can still stand. A design_gates row of
-- the step. The pair issues.ready_pending_step and issues.ready_pending_version of a READY the gate
-- refused on that step. A readmitted_approvals row whose step is `decisions`, the approval a
-- re-admission kept of that step. This migration deletes the two kinds of row, clears the pair,
-- and narrows the three checks that admit the step, the two of 20261105201323_design_gate_step
-- (design_gates.step and issues.ready_pending_step) and the one of
-- 20261106150254_readmitted_approval_step (readmitted_approvals.step), to `criteria` and `spec`.
--
-- The pair is cleared together, both halves in one statement, because issues_ready_pending_shape
-- (20261105201323) holds one half null exactly when the other is: a statement that nulled the step
-- alone would be refused. Clearing it drops the refused READY that waited on the `decisions` step,
-- and none is moved to another step: the step it waited on is one no deployment gates, so nothing
-- would ever release it. The rows go before the checks narrow, since a narrowed check refuses the
-- rows it would find.
--
-- This is a migration and not a step of every boot, as Dispatch's own deletions of the step are:
-- the daemon's store records no build announcement a boot step could read, and the change that
-- carries this file merges only once every pinned daemon runs the release that retired the step.
-- That is the daemon a rollback reaches, and it writes the step nowhere and reads none of what
-- this deletes, so it runs over the narrowed checks unchanged and rolling back past this migration
-- is an image rollback alone. Migrations here are forward-only and this one has no down file.
delete from design_gates where step = 'decisions';
delete from readmitted_approvals where step = 'decisions';
update issues set ready_pending_step = null, ready_pending_version = null
  where ready_pending_step = 'decisions';

alter table design_gates
  drop constraint design_gates_step_check,
  add constraint design_gates_step_check check (step in ('criteria', 'spec'));
alter table issues
  drop constraint issues_ready_pending_step_check,
  add constraint issues_ready_pending_step_check check (ready_pending_step in ('criteria', 'spec'));
alter table readmitted_approvals
  drop constraint readmitted_approvals_step_check,
  add constraint readmitted_approvals_step_check check (step in ('criteria', 'spec'));
