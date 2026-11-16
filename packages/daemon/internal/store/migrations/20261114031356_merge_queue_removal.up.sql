-- 20261114031356_merge_queue_removal.up.sql — the two outbox kinds that tell an issue's merger its
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
-- The check is one list, so the migration that sets it names every kind, as 20261110235320 and
-- 20261113082214 did: the latter's twenty-two kinds, which are the kinds the daemon writes,
-- update_ask among them, and these two. Its version stands above 20261113082214 on purpose.
-- Migrate applies every unrecorded migration in version order, so the list with the higher
-- version is the last word on any database. Numbered below it, this list would have dropped
-- update_ask from a database main had migrated, and main's list would have dropped these two
-- kinds from a fresh one. No release carries the version this branch first gave the file
-- (20261113072437), so no deployment's database records it. Only a database that a build of this
-- branch migrated before the forward merge does, and that database records neither of the update
-- round's migrations, so it applies 20261113082214 before this file. That migration's list names
-- neither kind of this one, so a queued merge_queue_read or merge_queue_removal row stops the
-- upgrade there (SQLSTATE 23514) and this file never runs. Delete those rows first, with the
-- statement of docs/kubernetes.md's rollback of this release; each costs a merger one message
-- about one removal. With none of them queued, the upgrade reaches this file, which writes the
-- check whole.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal'));
