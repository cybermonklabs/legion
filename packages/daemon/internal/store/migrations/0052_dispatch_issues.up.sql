-- 0052_dispatch_issues.up.sql -- the daemon's mirror of what Dispatch says about every issue of
-- its project: the status it stands at, the keys it waits on, and the sequence of the newest
-- reading of Dispatch the row took (INFRA-225). Each value also keeps the sequence of the reading
-- that wrote it, since 20261120055717_dispatch_issue_value_sequences.
--
-- Admission decides whether to admit a root, and the workflow whether to start a child's first
-- phase, inside one transaction that does no I/O, so neither can ask Dispatch what an issue is
-- blocked by at the moment it decides. This table is what they read instead: every issue of the
-- project, recorded or not, since a child can name a blocker the daemon has never admitted.
--
-- Dispatch has no issue delete, and never moves an issue between projects, so no key ever stops
-- existing and none ever changes its project, and nothing in the daemon re-projects a row. A
-- reading changes a value of a row only at a higher sequence than that value's own
-- (20261120055717_dispatch_issue_value_sequences). Rows are removed only by a boot that takes its
-- project's refill mark: the first boot of a project on the release of that migration, and the
-- first boot after that migration's way back marks the project again, empties that project's
-- rows and fills them again from its listing.
create table dispatch_issues (
    key text primary key,
    project text not null,
    status text not null,
    blockers text[] not null default '{}',
    dispatch_seq bigint not null
);

-- Every consumer reads one project's whole mirror in a single query and judges in memory, and
-- every one of those reads is `where project = $1 order by key`, so the index carries the key as
-- well: the ordering comes off the index instead of a sort of the project's rows.
create index dispatch_issues_project on dispatch_issues (project, key);
