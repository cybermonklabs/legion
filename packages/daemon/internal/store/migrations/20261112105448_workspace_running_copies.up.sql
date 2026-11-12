-- 20261112105448_workspace_running_copies.up.sql — the copy of an issue workspace's unpushed work
-- the daemon takes while the pod runs, one row per issue, held apart from the bundle an idle
-- release saves (INFRA-1117).
--
-- It is a table of its own and never a column on workspace_bundles, because the presence of a row
-- in that table is itself a decision. store.workspaceBundleUnrestored reads nothing but that
-- presence and workflow.IdleRelease turns it into a hold (IdleHoldBundleUnrestored), so a running
-- copy written there would stop every idle release of every working issue: a copy is taken while
-- the roles work, every few minutes, and no pod restores one while the pod it came off still
-- lives, so the hold would never lift. Writing it there would also put a copy of a live disk's
-- work over a release bundle whose restore no pod has confirmed, which is the only copy of work
-- no disk holds at all.
--
-- The rollback decides it as firmly. Migrations are forward only, so a daemon of an earlier
-- release put back over this one meets whatever rows it finds: a running copy left in
-- workspace_bundles would read to that daemon as an unrestored release bundle and hold those pods
-- for ever, with no code in it that could ever delete one. This table is read by no earlier daemon
-- at all, so the same rollback leaves its rows where they are and holds nothing.
--
-- The row carries the shape a runtime.WorkspaceBundle carries, as workspace_bundles holds it:
-- bundle is the git bundle's bytes and size_bytes their length, at is when the shim took the copy,
-- head the commit the working copy sat on, tips the commit ids of the heads the bundle carries and
-- bookmarks each local bookmark that pointed into the saved set with its commit, since a bundle
-- that only unbundles leaves every saved head out of jj's view.
--
-- source_pod_uid and the attempt columns are the same rule the release bundle keeps, for the same
-- reason (store.TakeRunningWorkspaceCopyForRestore). source_pod_uid is the pod the work was taken
-- off: that pod's own disk still holds the work, and its roles container restarts on that disk, so
-- a copy offered back to it would `jj edit` the workspace back to the copy's head and move each
-- bookmark `--allow-backwards` over everything done since. attempt_pod_uid, attempt_at and
-- attempt_outcome record the one attempt the copy gets, written before a byte leaves the daemon,
-- so a send that failed halfway, a restore that failed and a restore whose report never arrived
-- all leave a row no second pod is sent.
--
-- restore_id is the id the restore carries to the pod's launcher and reads back out of the report
-- that confirms it (shimwire.LauncherBundle.Release), and it is drawn from a sequence of this
-- table's own so that the id alone says which kind of copy it names. A launcher holds its report
-- until some connection carries it, so a report can reach the daemon after a restart, with nothing
-- but its id to route it on; and the launcher refuses a slice whose release is not positive
-- (shimwire.LauncherBundle.Validate), so the negative half of the space is not available to mark
-- the kind with. The sequence starts at 4611686018427387904 (2^62) instead, and the same migration
-- holds issue_releases.id below that number, so the two id spaces are disjoint by the schema
-- rather than by hope: a report below the boundary names a release bundle, one at or above it
-- names a running copy, and no growth of either sequence can make an id mean both.
-- store.MinRunningWorkspaceCopyRestoreID is that boundary in the daemon.
create sequence workspace_running_copy_restore_ids as bigint
  start with 4611686018427387904 minvalue 4611686018427387904;

create table workspace_running_copies (
    issue           text primary key,
    restore_id      bigint      not null unique default nextval('workspace_running_copy_restore_ids'),
    bundle          bytea       not null,
    size_bytes      bigint      not null,
    at              timestamptz not null,
    head            text        not null,
    tips            jsonb       not null default '[]'::jsonb,
    bookmarks       jsonb       not null default '{}'::jsonb,
    source_pod_uid  text        not null check (source_pod_uid <> ''),
    attempt_pod_uid text,
    attempt_at      timestamptz,
    attempt_outcome text,
    constraint workspace_running_copies_attempt_whole
      check ((attempt_pod_uid is null) = (attempt_at is null)),
    constraint workspace_running_copies_attempt_outcome
      check (case when attempt_pod_uid is null then attempt_outcome is null
                  else attempt_outcome in ('sent', 'refused_older') end)
);

alter sequence workspace_running_copy_restore_ids owned by workspace_running_copies.restore_id;

alter table issue_releases
  add constraint issue_releases_id_below_running_copy_restore_ids check (id < 4611686018427387904);
