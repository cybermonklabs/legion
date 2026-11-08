-- 20261108000545_claim_release.up.sql — the outbox kind that releases an issue's Dispatch claim
-- (claim_release).
--
-- When the daemon takes an issue out of the workflow without closing it, the issue keeps the
-- Dispatch claim of the session the daemon stopped, so the board shows a parked issue as owned
-- and in work. Dispatch releases a claim on the close alone, by its own rule, so the release is
-- the daemon's. It rides the outbox like every other Dispatch write, so a Dispatch that is down
-- does not fail the transaction that parked the issue; the row is retried until the claim is
-- gone, or until an hour has passed since it was written. The row carries the issue key alone:
-- the holder and the issue's record are read when the row runs, not when it is queued.
--
-- The check is one list, so the migration that sets it names every kind, as 0049, 20261101222956
-- and 20261106041346 did: whichever of two open branches merges last admits both their kinds.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_wake', 'check_look', 'check_bound',
  'phase_ended', 'issue_bundle', 'issue_release', 'gated_steps_publish', 'claim_release'));
