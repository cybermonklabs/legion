-- 20261107095842_outbox_kinds_without_the_merge_lane.up.sql — the outbox's kinds once the daemon
-- merges nothing (dispatch://INFRA-714 R2) and the production check names its own wake
-- (INFRA-809).
--
-- Two releases wrote this constraint and neither names the set the daemon now writes.
-- 20261105081500 named this release's kinds with the idle release's two, and it still allowed
-- `merge_queue_publish`, `auto_merge_disable` and `ready_merge` nowhere — it already dropped them
-- — but named `check_compare`, which INFRA-809 renamed. 20261105231916 then named `check_wake`
-- and brought the three merge-lane kinds back, since it was written on a branch where the daemon
-- still merged, and it left `role_message` out, since that kind is this release's own. A database
-- ending at either one therefore refuses a row the daemon writes, or allows a row nothing writes.
--
-- This migration names the kinds the merged daemon writes, as 20261105081500 did for the two
-- before it — this release's seventeen, with `check_wake` for the compare INFRA-809 retired and
-- `gated_steps_publish`, which INFRA-1272 added after both of them — and `claim_release`, which
-- main's 20261108000545 added. That kind is named here although this release's daemon queues no
-- such row, because a database main migrated has recorded that version and not this one, so an
-- upgrade runs this list over a park's pending claim-release row, which a park queues and retries
-- for up to an hour: a list without it fails the upgrade on the row (SQLSTATE 23514) and the
-- daemon cannot boot. 20261108152000 is the last word of both lists. It is safe on a database
-- ending at either earlier version, and on one that already carries a constraint of this name from
-- any of them, because it drops the constraint if it exists before adding it. No row is deleted,
-- and none of a retired kind is left to refuse: main still writes `ready_merge`,
-- `auto_merge_disable` and `merge_queue_publish`, so a database of that release can carry such a
-- row, and 20261029120800 of this release deletes every one of them earlier in the same run.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release'));
