-- 20261111153000_issue_facts_computed.up.sql
--
-- What the Legion facts consumer's last whole read of each project answered
-- (dispatch://INFRA-2915). The consumer reads a project whole (legionfacts.ComputeProject) the
-- first time a build works that project, and after that it computes the issues its feeds name.
-- This table is the record that the whole read happened and of how many rows it wrote.
--
-- Before it, "this project has never been computed" was read from "this project holds no
-- issue_facts row", which a project whose issues carry no Legion phase work can never clear: the
-- computation of such a project answers no row, so the pass writes none, and the next pass reads
-- the whole project again, every legionfacts.SweepEvery for as long as the project holds no
-- worked issue. A whole-project read is the cost this consumer exists to remove, so the mark is
-- written rather than inferred from an absence.
--
-- The mark is not a row of issue_facts_cursor (20261111090000). That table holds one row per
-- named feed and a position in it: a number the next pass reads on from. A project's whole
-- computation has no position. It is a fact about a project, keyed by the project, read as a yes
-- or a no, so a row of it would leave `name` holding two kinds of thing and `position` meaning
-- nothing for one of them.
--
--   project_key     the project, and the row's identity: one mark per project.
--   computed_at     the moment the pass that read the project whole computed as of. It is
--                   written in the same transaction as that pass's rows and its cursors, so a
--                   pass that failed leaves no mark and the next pass reads the project whole
--                   again.
--   computed_rows   how many rows that whole read answered. A project whose mark counts rows
--                   and which now holds none has lost them to something outside this consumer,
--                   and is read whole again; a project whose mark counts none is the project
--                   with no worked issue, and is left alone. Both answers settle after one
--                   whole read, because the read writes the count it answered.
--
-- One new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so the two builds roll together (cmd/dispatch/README.md,
-- "Database migrations"). The census answers 0, and rolling back past it is an image rollback
-- alone. A database that already holds issue_facts rows holds no mark, so the first pass after
-- this migration reads each of its projects whole once and marks it: one pass, not a pace.
create table issue_facts_computed (
    project_key text primary key,
    computed_at timestamptz not null,
    computed_rows integer not null
);
