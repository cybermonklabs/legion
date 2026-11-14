-- 20261114090103_workspace_restore_asks.up.sql — when each pod of an issue first asked for a
-- restore of the issue's saved workspace copies, by the database's clock (INFRA-1117).
--
-- The restore decides whether a copy is sent to a pod by comparing the copy's `at` with the pod's
-- first ask (runtime.DecideWorkspaceRestore), and both are this database's clock: a copy's `at`
-- is when the store wrote it (store.Store.PutRunningWorkspaceCopy, store.Store.PutWorkspaceBundle),
-- and first_asked_at is when the store recorded the pod's first ask, in the transaction that takes
-- the copies for it (store.Store.TakeWorkspaceCopyForRestore). Both are written with
-- clock_timestamp() under one lock per issue, which every write of a copy's time and every take
-- holds first (store's lockWorkspaceCopies), so a copy written before a pod's first ask has
-- committed before that ask reads the copies, and one written after it carries a later time; the
-- column has no default, since its one writer names that time. The pod's time was its
-- creationTimestamp before, which the Kubernetes API server stamps by its own clock, so a pod that
-- clock stamped seconds before a copy the database wrote was refused that copy and spent its one
-- attempt, and no pod after it was sent the work. Every pod's launcher asks at its first role
-- start, so a pod that existed before a copy was written asked before it. A copy written after a
-- pod's first ask is sent to no ask of that pod and nothing is recorded on it, so it stays for the
-- pod after it.
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
