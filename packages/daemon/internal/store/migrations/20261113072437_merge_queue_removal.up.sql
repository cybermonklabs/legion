-- 20261113072437_merge_queue_removal.up.sql — the two outbox kinds that tell an issue's merger its
-- pull request left the merge queue: the daemon's one read of the pull request after GitHub's
-- `dequeued` (merge_queue_read), and the message to the merger that read's answer writes
-- (merge_queue_removal).
--
-- The event is acknowledged once its transaction commits, so the read is a durable row rather than
-- a read held in memory: a restart between the event and the read loses nothing. The message is a
-- row of its own, written once the read has answered, because it waits until the merger's session
-- holds its role, and a row that read GitHub too would read it again at each of those waits. Both
-- ride the role-message lane (internal/record's lanes) and hold the issue's pod (the store's
-- idleWaitingKinds).
--
-- The check is one list, so the migration that sets it names every kind, as 20261110235320 did:
-- its twenty-one kinds, which are the kinds the daemon writes, and these two.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish',
  'merge_queue_read', 'merge_queue_removal'));
