-- 20261118172458_bundle_size_refusals.up.sql -- a release whose save found the work over the
-- bound a release carries is not marked again on the clock: the pod is held until the rule
-- workflow.IdleHoldBundleTooLarge states ends the hold (dispatch://INFRA-3136).
--
-- workspace_bundle_refusals is the last save of an issue's release that found the workspace's
-- unpushed work over the bound a release carries, one row per issue: the moment that release was
-- marked, and the head of the issue's pull request then ('' with no pull request). The idle rule
-- reads it (store's readIdleView, workflow.IdleRelease) and compares the two with the claims of
-- the pod and the head now, as workflow.IdleHoldBundleTooLarge states. A save that failed for a
-- reason of its own writes no row and is tried again on the next tick, since no push or turn
-- cures such a cause. The save writes the row before its cancel (store.RecordBundleRefusal), so a
-- restart reads it; a save that stores the work, or finds none to store, deletes it (store's
-- PutWorkspaceBundle and DeleteWorkspaceBundle), and so does the end of the issue's work, which
-- deletes the bundle.
--
-- The table was first written into 20261112215432_delivery_resume, whose resume of an answered
-- root it has nothing to do with, and it moved here before any release carried it. A database
-- migrated from this branch while the table stood there already holds it, so the statement creates
-- it only where it is missing, and the table it finds has the same columns.
--
-- There is no down file: migrations here are forward-only. A daemon of an earlier release neither
-- reads nor writes the table, so it runs on this schema as it did before and marks the release on
-- the clock as it always did. A rollback leaves the table in place, and this release, pinned
-- again, reads a row the earlier release left unchanged: the hold such a row sets ends by the
-- rule workflow.IdleHoldBundleTooLarge states, which reads the claims and the head the earlier
-- release went on writing.
create table if not exists workspace_bundle_refusals (
  issue text primary key,
  at timestamptz not null,
  head text not null
);
