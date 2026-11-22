-- 20261117000528_outbox_kinds_with_the_step_record.up.sql — the outbox kind that writes one start
-- or one end of a step of an issue onto Dispatch's record of that step (step_record).
--
-- The row rides a lane of its own (internal/record's LaneRecord), so a Dispatch that is slow to
-- take a record, or that backs one off until it has the route, holds no other row of the issue,
-- and it does not hold the issue's pod (it is not among the store's idleWaitingKinds): a record is
-- history, and the work it records never waits for it.
--
-- The check is one list, so the migration that sets it names every kind, as 20261116165926 did:
-- its twenty-seven kinds, which are the kinds the daemon writes, role_holders_publish and the two
-- reach notice kinds of 20261116082000 among them, and this one. Its version stands above
-- 20261116165926 and 20261116082000 on purpose: Migrate applies every unrecorded migration in
-- version order, so the list with the higher version is the last word on any database, and a list
-- that left out role_holders_publish would refuse every publication of an issue's role sessions,
-- and one that left out a reach notice kind every notice of a claim out of reach, and fail the
-- upgrade of a database that holds one pending. A rollback leaves the name in the check, which
-- refuses nothing an earlier release writes (docs/kubernetes.md, "Rolling back the release that
-- records each step").
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'reach_notice',
  'reach_controller_notice', 'step_record'));
