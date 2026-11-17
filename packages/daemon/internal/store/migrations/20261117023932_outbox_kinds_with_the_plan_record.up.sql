-- 20261117023932_outbox_kinds_with_the_plan_record.up.sql — the outbox kind that writes the plan one
-- run of an issue carries out onto Dispatch (plan_record), when the run's work starts.
--
-- The row rides the record lane (internal/record's LaneRecord) beside the step records, so a
-- Dispatch that is slow to take a plan, or that backs one off until it has the route, holds no other
-- row of the issue, and it does not hold the issue's pod (it is not among the store's
-- idleWaitingKinds): a record is history, and the work it records never waits for it.
--
-- The check is one list, so the migration that sets it names every kind, as 20261117000528 did:
-- its twenty-five kinds, which are the kinds the daemon writes, and this one. Its version stands
-- above 20261117000528 on purpose: Migrate applies every unrecorded migration in version order, so
-- the list with the higher version is the last word on any database. A rollback leaves the name in
-- the check, which refuses nothing an earlier release writes (docs/kubernetes.md, "Rolling back the
-- release that rates each plan").
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'step_record', 'plan_record'));
