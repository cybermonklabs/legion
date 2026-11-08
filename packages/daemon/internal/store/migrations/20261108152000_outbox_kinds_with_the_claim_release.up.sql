-- 20261108152000_outbox_kinds_with_the_claim_release.up.sql — the outbox's kinds once the daemon
-- merges nothing (dispatch://INFRA-714 R2) and releases a parked issue's Dispatch claim
-- (claim_release, 20261108000545).
--
-- The check is one list, so whichever of two open branches merges last names both their kinds,
-- which is what 20261108000545 said of itself. It merged first, from a branch where the daemon
-- still merged, so it names `merge_queue_publish`, `auto_merge_disable` and `ready_merge`, which
-- this release writes nowhere and whose rows 20261029120800 deletes, and it leaves out
-- `role_message`, this release's own kind. A database ending there refuses every role message the
-- daemon writes, which is how a gate's opening reaches a worker.
--
-- This migration is the last word of both: this release's eighteen kinds with `claim_release`. It
-- is safe on a database ending at either one, since it drops the constraint if it exists before
-- adding it, and it deletes no row.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release'));
