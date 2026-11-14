-- 20261114090103_workspace_restore_asks.up.sql — when each pod of an issue first asked for a
-- restore of the issue's saved workspace copies, and when each saved copy was written, by the
-- database's clock (INFRA-1117).
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
-- of its own, written_at, and every row already standing takes its at, which was the write by this
-- branch's store and the shim's reading by an earlier release's. workspace_running_copies.written_at
-- has no default, since every writer of that table names it. workspace_bundles.written_at defaults
-- to clock_timestamp() for an earlier daemon after a rollback, which saves release bundles naming
-- no such column and would otherwise fail every save. The rollback deletes every release bundle
-- (docs/kubernetes.md, "Rolling back idle release") and the earlier daemon records no first ask,
-- so every written_at it leaves falls inside its run and every first ask the table below holds
-- falls before that run or after it: a later save of the same issue by the earlier daemon, which
-- leaves the row's first written_at in place, is ordered against every such ask as its own write
-- would be.
--
-- One row per pod that asked, keyed by the issue and the pod's uid. The first ask inserts it and
-- every later ask of the same pod leaves it as it is (insert ... on conflict do nothing), so
-- first_asked_at never moves. Each issue keeps its 16 newest rows by first_asked_at, and the first
-- ask of a pod deletes the rest: an issue has one pod at a time, and two while one replaces the
-- other, so a pod whose row goes has long been gone. A pod whose row went and that asked again
-- would be recorded anew, at that ask, and could then be sent a copy written since its true first
-- ask.
--
-- Nothing of an earlier release reads or writes this table, so a rollback to one needs no act on
-- it, and its rows stay: deleted, every pod that lives through the rollback would be recorded at
-- its first ask after the roll forward, and a copy written in between could then reach a pod that
-- has been working since before it (docs/kubernetes.md, "Rolling back the running copy"). The rows
-- are a pod uid and a time each, at most 16 per issue, and they outlive the issue's close.
create table workspace_restore_asks (
    issue          text        not null,
    pod_uid        text        not null,
    first_asked_at timestamptz not null,
    primary key (issue, pod_uid)
);

alter table workspace_running_copies add column written_at timestamptz;
update workspace_running_copies set written_at = at;
alter table workspace_running_copies alter column written_at set not null;

alter table workspace_bundles add column written_at timestamptz;
update workspace_bundles set written_at = at;
alter table workspace_bundles
  alter column written_at set not null,
  alter column written_at set default clock_timestamp();
