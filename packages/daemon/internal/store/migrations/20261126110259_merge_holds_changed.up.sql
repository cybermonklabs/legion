-- 20261126110259_merge_holds_changed.up.sql — the outbox kind that tells the merger of an issue in
-- merging that the project's merge holds document changed (merge_holds_changed).
--
-- Each project's merge holds are one Dispatch document of the project, merge-holds-md, which only
-- the controller writes. A merger that a hold stops ends its turn and subscribes to nothing, so
-- the daemon tells it of each event of the document: the architect of an issue in accepting by a
-- notice, and the merger of an issue in merging by a row of this kind, since a notice reaches only
-- an architect. The row wakes a released merger and waits until the merger's session holds its
-- role. It rides the role-message lane (internal/record's lanes) and holds the issue's pod (the
-- store's idleWaitingKinds).
--
-- The check is one list, so this migration names every kind, as 20261122154643 did: its
-- thirty-two kinds, in its order, and then this one, thirty-three in all. Its version stands above
-- 20261122154643, the list with the two basis seeds, 20261119203047, the design news's list,
-- 20261119003157, the list with the conflict's second read and the merge forward, 20261117023932,
-- the plan record's, and 20261117000528, the step record's, so on any database this list is the
-- last word: Migrate applies every unrecorded migration in version order, and at a lower version
-- any of those lists, none of which names merge_holds_changed, would be applied after this one and
-- refuse the kind.
--
-- This list must name step_record, plan_record, conflict_confirm, merge_forward, design_changes,
-- phase_ended, pending_basis_seed and reparent_seed, because a database main's release migrated
-- can hold a row of each of those kinds when this migration is applied over it, and a list without
-- the kind would fail its ADD CONSTRAINT on that row (SQLSTATE 23514), and Migrate with it
-- (store's TestADatabaseMainMigratedWithRowsOfItsNewestKindsUpgrades). Main's daemon queues all
-- eight: phase_ended again since 20261122154643, for the admission pass's move of a parent whose
-- worker is in the middle of its phase into awaiting_children, and the two basis seeds for its
-- reads of a root's design-gate basis and of a re-parented root. A list without one of them would
-- also refuse the first such row this release queues, and the transaction that queued it with it.
-- On a fresh database the basis seeds' list is applied first and this one after it, so a list here
-- without pending_basis_seed and reparent_seed would refuse every read of a root's design-gate
-- basis and of a re-parented root.
--
-- This migration was numbered 20261117113130 on earlier builds of this branch, and 20261119211852
-- from the forward merge of 2026-11-19 until the forward merge of 2026-11-26 brought in
-- 20261122154643, which stood above it and left the kind out. No deployment ran either version.
-- Each stands below a list that does not name merge_holds_changed, so on a fresh database that
-- list would have been the last word and refused the kind, and 20261119211852 was applied after
-- 20261122154643 on a database main migrated, where its list, without the two basis seeds, would
-- have refused a pending row of either. A database a build of this branch migrated before this
-- merge records an older version, and it is a test's own, to make again.
--
-- The list names retention_end too, the kind dispatch://INFRA-4730's 20261122154645 adds, which
-- stands below this version: on a fresh database, and on one a build of that branch migrated,
-- this list is applied after that one and is the last word, so a list without retention_end would
-- refuse every end of an issue's retention, and fail the upgrade over a pending one. Thirty-four
-- kinds in all. On a database main migrated before that branch's merge, 20261122154645 is applied
-- after this one, and its list names merge_holds_changed for that reason.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'retention_end'));
