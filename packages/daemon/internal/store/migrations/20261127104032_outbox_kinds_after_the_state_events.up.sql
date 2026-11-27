-- 20261127104032_outbox_kinds_after_the_state_events.up.sql — the outbox's kind check once more,
-- above dispatch://INFRA-3931's state events (20261126163320), with the kinds of every list
-- below it: the merge's union of main's state events and dispatch://INFRA-4730's retention end.
--
-- The check is one list, so the migration that sets it names every kind: 20261126163320's
-- thirty-seven, its three state kinds (state_change, state_admission, state_boot), merge_holds_changed
-- and dispatch://INFRA-4730's retention_end among them. 20261122154645_issue_facts and
-- 20261126163320 name these same kinds (each file says why), so on every database the check
-- stands as this list whichever of them was applied last, and a row of any of them pending at the
-- upgrade is kept through it: the constraint validates every row the table holds, and a kind left
-- out would fail the upgrade over its pending row.
--
-- Its version is what the merge needs beside the list. Migrate applies every migration a database
-- has not recorded, below its newest version as above it (store.Migrate), and the store reports
-- its schema version as the newest it recorded (store.SchemaVersion), which each boot's state
-- event carries (stateevent.Boot). A database main migrated at #4432 records 20261126163320 as its
-- newest, and dispatch://INFRA-4730's migrations stand below it, so without this one the first boot
-- of the merge would announce the schema version the earlier daemon announced. Dispatch reads a
-- daemon whole at a boot whose schema version differs from the one it read last (its state
-- intake's movesSchema), and at no other boot. The earlier daemon's state events named each
-- issue's tree as its record stored it; this release names the tree the Dispatch chain gives
-- (record.ObservedTrees, R22 of dispatch://INFRA-3320). So this version, above every other, is what
-- makes the first boot of the merge read whole on every database it upgrades, and Dispatch then
-- holds the chain's trees from that read on.
--
-- It carries no way back: the earlier release writes no kind this list leaves out. A rollback to
-- that release keeps this version recorded, so the earlier daemon's boot announces this same
-- schema version and Dispatch reads it whole at no boot; the guide's way back says how its trees
-- are read again (docs/kubernetes.md, "Rolling back the release that stores no tree").
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'retention_end', 'state_change', 'state_admission', 'state_boot'));
