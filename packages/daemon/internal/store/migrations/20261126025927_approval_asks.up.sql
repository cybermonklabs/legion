-- 20261126025927_approval_asks.up.sql — the asks of each project's issues, each issue's assignee
-- and the project's default assignee, kept for the approvals signal of admission by signals.
--
-- The approvals signal is the count of the asks open on a project's issues that wait on a person,
-- that person being the project's default assignee: an ask waits on its asked_of person, else its
-- issue's assignee, else the project's default assignee (signals.WaitsOnDefault), and an ask's
-- turn is what its events leave it (signals.NextAsk). Admission keeps each in the fact's own
-- transaction, before that fact's admission pass (admit's keepSignalFacts), from Dispatch's events
-- as the Dispatch consumer decodes them and from the boot's read of Dispatch (intake.ApprovalsBootRead):
--
--   - approval_asks is one row per ask of a project's issues: its issue, whether it is open,
--     whether it waits on a person rather than an agent, the person its asked_of names ('' for
--     none), and the sequence, in its issue's Dispatch event log, of the event that last changed
--     it (for an ask the boot read, the sequence its issue stood at when the read began). A closed
--     ask keeps its row, so an older event of it delivered later (a redelivery) is refused by that
--     sequence rather than taken for a new ask. A read of the signal reads the open asks alone, by
--     the partial index, so it is bounded by the asks open and never by the project's history.
--   - approval_assignees is one row per issue of a project whose assignee an event or the boot's
--     listing named: the person ('' for none) and the sequence of the issue's event, or of its
--     listing, that named it.
--   - approval_default_assignees is one row per project whose default assignee is known: the person,
--     the sequence of Dispatch's settings.project_default_assignee.updated event that set it in the
--     project's own event log (0 where only the boot's read of Dispatch gave it, which carries no
--     sequence of that log), and the time of that event or of that read. A project with no row has
--     no default assignee known, and its approvals signal reads as not recorded, never as 0.
--
-- An event is kept only where its sequence is above the one its row holds, so an event delivered
-- twice or after a newer one changes nothing; the boot's read of an ask or an assignee where its
-- sequence is at least that one, and of the default assignee where no settings event was kept since
-- the read began. Each row so ends at the newest state Dispatch recorded, whatever order the stream
-- and the boot's read bring them in.
--
-- Every time is written from Dispatch's own times, never by a column default.
--
-- The migration adds three tables and changes none that an earlier release reads, so a daemon of
-- the release before runs over the migrated database, as a rollback leaves it: it keeps nothing
-- here and moves no row.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon that shares the database stopped and before
-- the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. Dropping the
-- three tables, deleting the approvals row of signal_inputs and deleting this version row lets the
-- roll-forward apply this migration anew, record a fresh kept_since at its first boot, and read the
-- open asks, their issues and the default assignee again at that boot.
--
-- begin;
-- drop table if exists approval_asks;
-- drop table if exists approval_assignees;
-- drop table if exists approval_default_assignees;
-- delete from signal_inputs where input = 'approvals';
-- delete from schema_version where version = 20261126025927;
-- commit;
create table approval_asks (
    project  text    not null check (project <> ''),
    ask      text    not null check (ask <> ''),
    issue    text    not null check (issue <> ''),
    open     boolean not null,
    human    boolean not null,
    asked_of text    not null,
    seq      bigint  not null check (seq >= 1),
    primary key (project, ask)
);
create index approval_asks_open on approval_asks (project) where open;
create table approval_assignees (
    project  text   not null check (project <> ''),
    issue    text   not null check (issue <> ''),
    assignee text   not null,
    seq      bigint not null check (seq >= 1),
    primary key (project, issue)
);
create table approval_default_assignees (
    project  text        primary key check (project <> ''),
    assignee text        not null check (assignee <> ''),
    seq      bigint      not null check (seq >= 0),
    at       timestamptz not null
);
