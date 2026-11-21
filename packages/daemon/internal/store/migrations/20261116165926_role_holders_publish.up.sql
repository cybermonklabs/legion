-- 20261116165926_role_holders_publish.up.sql — the outbox kind that publishes the live role
-- sessions of an issue onto its Dispatch issue row (role_holders_publish).
--
-- Dispatch holds, per issue, the session that holds each Legion role (issue_role_holders). A later
-- release of Dispatch takes the role of a result from that record. The daemon is the one party
-- that gives a session a role, so it writes the record: at a claim's boot ready, in the
-- transaction of each claim write that changes the session holding a role, an end among them, and
-- at each boot (internal/store's putClaim, internal/daemon's roleHoldersChanged and
-- queueRoleHoldersPublish). Each is a row of this kind on the issue's board lane.
--
-- The check is one list, so the migration that sets it names every kind, as 20261114031356 did:
-- that migration's twenty-four kinds, which are the kinds the daemon writes, this one, and
-- reach_notice and reach_controller_notice, which 20261116082000_reach_notice_kinds adds. Its
-- version stands above 20261114031356 and 20261116082000 on purpose: Migrate applies every
-- unrecorded migration in version order, so the list with the higher version is the last word on
-- any database that applies both, and a database that recorded this list before
-- 20261116082000 takes that file's list, which names this kind too.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'reach_notice',
  'reach_controller_notice'));

-- One publish row per issue, which is what lets the enqueue be an upsert
-- (store.EnqueueRoleHolders): the row names the issue alone, and the executor reads the
-- live set when it runs, so a second claim change while the row waits makes the one row due again
-- rather than queueing beside it. The index holds role_holders_publish rows alone, so no other
-- kind's insert conflicts on it. The kind is new in this migration, so there is nothing to
-- collapse first.
create unique index outbox_role_holders_publish_issue_unique
  on outbox (issue)
  where kind = 'role_holders_publish';
