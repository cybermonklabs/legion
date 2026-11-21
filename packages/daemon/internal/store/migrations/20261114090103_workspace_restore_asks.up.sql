-- 20261114090103_workspace_restore_asks.up.sql — when each pod of an issue first asked for a
-- restore of the issue's saved workspace copies, when a role of that pod first registered, and
-- when each saved copy was written, by the database's clock (INFRA-1117).
--
-- The restore decides whether a copy is sent to a pod by comparing when the copy was written with
-- the pod's first ask (runtime.DecideWorkspaceRestore), and both are this database's clock: a
-- copy's written_at, added below, is when the store wrote it (store.Store.PutRunningWorkspaceCopy,
-- store.Store.PutWorkspaceBundle), and first_asked_at is when the store recorded the pod's first
-- ask, in the transaction that takes the copies for it (store.Store.TakeWorkspaceCopyForRestore).
-- Both are written with clock_timestamp() under one lock per issue, which every write of a copy
-- and every take holds first (store's lockWorkspaceCopies), so a copy written before a pod's first
-- ask has committed before that ask reads the copies, and one written after it carries a later
-- time; first_asked_at has no default, since its one writer names that time. The pod's time was its
-- creationTimestamp before, which the Kubernetes API server stamps by its own clock, so a pod that
-- clock stamped seconds before a copy the database wrote was refused that copy and spent its one
-- attempt, and no pod after it was sent the work. Every pod's launcher asks at its first role
-- start, so a pod that existed before a copy was written asked before it. A copy written after a
-- pod's first ask is sent to no ask of that pod and nothing is recorded on it, so it stays for the
-- pod after it.
--
-- A copy's at is no longer that write: it is the database's clock when the copy's take began,
-- read before the shim's bundler read the workspace (store.Store.BeginWorkspaceCopy), which the
-- restore orders the issue's two copies by and compares with the newest recorded push. A copy read
-- before a push and written after the push's record would otherwise carry a time after the push
-- and be sent over the pushed commits. So each of the two copy tables keeps the write as a column
-- of its own, written_at. Every row already standing takes this database's clock at the upgrade,
-- clock_timestamp(), and not its at, which on an earlier release is the shim's reading by its
-- node's clock: workspace_restore_asks is created empty above, so every standing copy was written
-- before every first ask the restore will read, which is exactly what that time records.
-- workspace_running_copies.written_at has no default, since every writer of that table names it.
-- workspace_bundles.written_at defaults to clock_timestamp() for an earlier daemon after a
-- rollback, which saves release bundles naming no such column and would otherwise fail every save.
-- A rollback to an earlier release keeps the release bundles, and that daemon's save, an upsert
-- naming no such column, leaves a row's written_at at the row's first insert. Such a time is never
-- ordered wrongly against a first ask: every ask the table holds was recorded by this release,
-- before the rollback or after the roll forward, and an issue has one pod at a time, and two while
-- one replaces the other. So a pod that asked before the rollback is either the pod a later save of
-- the earlier daemon came off, which is sent nothing of that save, or a pod long gone, and a pod
-- made while the earlier daemon ran first asks after the roll forward, after every write of that
-- daemon's (docs/kubernetes.md, "Rolling back the running copy").
--
-- One row per pod that asked, keyed by the issue and the pod's uid. The first ask inserts it and
-- every later ask of the same pod leaves it as it is (insert ... on conflict do nothing), so
-- first_asked_at never moves. Each issue keeps its 16 newest rows by first_asked_at, and the first
-- ask of a pod deletes the rest: an issue has one pod at a time, and two while one replaces the
-- other, so a pod whose row goes has long been gone. A pod whose row went and that asked again
-- would be recorded anew, at that ask, and could then be sent a copy written since its true first
-- ask.
--
-- registered_at is when a role of that pod first registered with the daemon, null until one does
-- (store.Store.RecordPodRegistration, which the daemon calls before it answers a registration).
-- A copy sent to a pod stays unspent while that pod's row holds no registration: the copy was
-- sent before any role of the pod ran, so a pod, or its roles container, that ends before any
-- role registered has changed nothing of the workspace, and the next start of the issue is sent
-- the copy again, in that pod's restarted container or in the pod after it. Once a role has
-- registered it may have changed the workspace, and a second restore there would move that work
-- back, so the copy is spent from then on (store's heldCopy). The record is the pod's and not a
-- container's, so a restarted roles container of a pod where a role registered is sent nothing.
-- A copy sent to a pod whose row is gone is spent, as every sent copy was before.
--
-- Nothing of an earlier release reads or writes this table, and its rows stay through a rollback
-- to one: deleted, every pod that lives through the rollback would be recorded at its first ask
-- after the roll forward, and a copy written in between could then reach a pod that has been
-- working since before it (docs/kubernetes.md, "Rolling back the running copy"). A role that
-- first registers while the earlier release runs is recorded by none, so the rollback marks every
-- pod that asked as one where a role registered, with
-- `update workspace_restore_asks set registered_at = first_asked_at where registered_at is null;`
-- beside its `delete from workspace_running_copies;`, which gives the earlier release's rule back:
-- a copy sent to a pod is spent. Without it a release bundle, which the rollback keeps, sent to a
-- pod before any role of that pod registered and whose roles then registered under the earlier
-- release, would read as unspent after the roll forward, and that pod's restarted roles container
-- would be sent it over the roles' work. Every copy written after the rollback was written after
-- the first ask of each pod that lived through it, so the late-copy rule leaves it for the pod
-- after (runtime.DecideWorkspaceRestore). The rows are a pod uid and two times each, at most 16
-- per issue, and they outlive the issue's close.
create table workspace_restore_asks (
    issue          text        not null,
    pod_uid        text        not null,
    first_asked_at timestamptz not null,
    registered_at  timestamptz,
    primary key (issue, pod_uid)
);

alter table workspace_running_copies add column written_at timestamptz;
update workspace_running_copies set written_at = clock_timestamp();
alter table workspace_running_copies alter column written_at set not null;

alter table workspace_bundles add column written_at timestamptz;
update workspace_bundles set written_at = clock_timestamp();
alter table workspace_bundles
  alter column written_at set not null,
  alter column written_at set default clock_timestamp();
