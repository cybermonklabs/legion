-- 20261126190955_outbox_kinds_with_the_queue_timeline_read.up.sql — the outbox kind that reads a
-- pull request's merge-queue timeline for the queue signal of admission by signals
-- (queue_timeline_read).
--
-- A queue_timeline_read row reads one pull request's timeline from GitHub outside the fact lock,
-- after GitHub reported the pull request enqueued, dequeued or merged, and applies what it read as
-- intake.QueueTimeline, whose events admission keeps in queue_events (20261126020201). It rides
-- the signals lane (internal/record's lanes).
--
-- The check is one list, so this migration names every kind, as 20261126110259 did: its
-- thirty-three kinds, in its order, and then this one, thirty-four in all. Its version stands
-- above 20261126110259, the list with merge_holds_changed, and above every other migration that
-- writes the list, so on any database this list is the last word: Migrate applies every
-- unrecorded migration in version order, and a list at a lower version, none of which names
-- queue_timeline_read, would be applied after this one and refuse the kind. The kind is in no
-- migration of its own release but this one, so 20261126020201's way back and roll forward leave
-- the list as it stands: a migration that wrote the list again there, without merge_holds_changed,
-- would refuse on the roll forward every merge_holds_changed row the earlier release queued, and
-- Migrate with it (SQLSTATE 23514).
--
-- 20261126020201_queue_events wrote the list with queue_timeline_read on earlier builds of this
-- branch, until the forward merge of 2026-11-26 brought in 20261126110259, which stood above it
-- and left the kind out. No deployment ran it. A database a build of this branch migrated before
-- this merge records 20261126020201 with that list; its next upgrade applies 20261126110259, whose
-- list refuses a queued queue_timeline_read row, and it is a test's own, to make again.
--
-- A daemon of an earlier release refuses no row at the database, since this list admits every
-- kind it writes, and runs no queue_timeline_read row: none of its outbox lanes lists the kind
-- (record.LaneKinds), and its sweep wakes no lane for a kind in no lane
-- (record.Store.UnfinishedLanes). The way back of 20261126020201 deletes the queued rows of the
-- kind, and the rollback of the release that admits by signals (docs/kubernetes.md) runs it.
--
-- There is no down file: migrations here are forward-only.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'queue_timeline_read'));
