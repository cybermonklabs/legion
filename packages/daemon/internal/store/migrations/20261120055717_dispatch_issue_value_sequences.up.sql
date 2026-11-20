-- 20261120055717_dispatch_issue_value_sequences.up.sql — the copy of Dispatch's issues
-- (dispatch_issues) keeps, for each value of a row, the sequence of the reading that wrote it,
-- holds each issue's labels and board rank, has an index on its parent column, and marks each
-- project whose rows its first boot on this release refills (dispatch://INFRA-4729).
--
-- Each of five values of a row, its status, parent, blockers, labels and rank, keeps the sequence
-- of the reading of Dispatch that wrote it, in a column of its own (status_seq, parent_seq,
-- blockers_seq, labels_seq and rank_seq). A reading changes a value only at a higher sequence
-- than that value's own, so a reading at the value's own sequence changes nothing; a value at
-- sequence 0 was written by no reading, and any reading fills it. A reading writes only the values
-- it carries, and dispatch_seq stays the highest sequence of any reading the row took
-- (record.Store.PutDispatchIssue). The release before fenced the row as a whole on dispatch_seq
-- and wrote the parent outside that fence, so a reading at the row's own sequence could replace
-- or remove the parent the row held.
--
-- labels and rank are the issue's labels and its board rank as the reading showed them. No release
-- before this one wrote either, so each row holds them empty, at sequence 0. The index is on
-- parent alone: the children of a parent are read by it within one project
-- (record.Store.DispatchChildren), and a parent link never crosses a project.
--
-- A row the release before left takes its dispatch_seq as the sequence of its status, its parent
-- and its blockers, which is what that release's rule said of them: the reading at that sequence
-- wrote the row whole. At 0 any reading would replace them, an older one delivered late among
-- them, and put back a value Dispatch has moved past. That rule did not hold of every parent:
-- 20261103183833_issue_slots filled the parent of each row it found from the daemon's own records,
-- which no reading of Dispatch wrote, and a listing at the row's own sequence does not replace a
-- value at that sequence.
--
-- So each project the copy holds rows of gets a mark: a dispatch_issue_refills row whose
-- refilled_at is null. The first boot of that project on this release empties the project's rows
-- and fills them again from its listing, in the boot's own transaction and before its first
-- decision, and stamps refilled_at, which is the record that it did; a later boot finds the mark
-- stamped and refills nothing. That boot is the one place a row of the copy is removed
-- (0052_dispatch_issues). A row of no project ('') gets no mark, since no boot lists such a
-- project. Each table is one for every project of the database, and each boot takes only its own
-- project's mark.
--
-- Every new column has a default, so the release before, whose insert names key, project,
-- status, blockers, dispatch_seq and parent, still writes its rows. It names none of the new
-- columns, so each value it writes leaves that value's sequence behind it. A rollback to it
-- therefore runs the way back, the last paragraph of this header (migrations.WayBack), with every
-- daemon of the database stopped, before the earlier image is pinned: it marks each project that
-- has rows again, so that project's next boot on this release refills them. The refill makes no
-- call to Dispatch of its own: that boot deletes the project's rows and writes one row for each
-- issue of the listing every boot reads. Every daemon that shares the database moves together, on
-- the way up and on the way back, since the copy and its marks hold every project's rows.
-- docs/kubernetes.md, "Rolling back the release that fences each value of the copy of Dispatch's
-- issues", carries the same statement, and internal/store's
-- TestTheCopysRefillMarksComeFromTheMigrationAndTheWayBack runs this one.
--
-- insert into public.dispatch_issue_refills (project) select distinct project from public.dispatch_issues where project <> '' on conflict (project) do update set refilled_at = null;
alter table dispatch_issues
  add column labels text[] not null default '{}',
  add column rank text not null default '',
  add column status_seq bigint not null default 0 check (status_seq >= 0),
  add column parent_seq bigint not null default 0 check (parent_seq >= 0),
  add column blockers_seq bigint not null default 0 check (blockers_seq >= 0),
  add column labels_seq bigint not null default 0 check (labels_seq >= 0),
  add column rank_seq bigint not null default 0 check (rank_seq >= 0);
update dispatch_issues set status_seq = dispatch_seq, parent_seq = dispatch_seq, blockers_seq = dispatch_seq;
create index dispatch_issues_parent on dispatch_issues (parent);
create table dispatch_issue_refills (
  project text primary key check (project <> ''),
  marked_at timestamptz not null default now(),
  refilled_at timestamptz);
insert into dispatch_issue_refills (project) select distinct project from dispatch_issues where project <> '';
