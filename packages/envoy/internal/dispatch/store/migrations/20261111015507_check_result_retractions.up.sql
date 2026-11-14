-- 20261111015507_check_result_retractions.up.sql
--
-- One row per retracted result: the record that says a stored result of a `check` block is
-- wrong (dispatch result --retract). Nothing in check_results is updated or deleted, so both
-- records stay readable, each with its own reason and actor.
--   result_id    the retracted result, and this table's key. One retraction per result: a
--                second retraction of one result is refused by this primary key and by the
--                route before it.
--   reason       why the result is wrong, which the person or session who retracted it wrote.
--   session      the session that retracted it ('' for a person), as check_results records one.
--   actor        the actor that retracted it, in the shape check_results and every other table
--                stores an actor in.
--   created_at   when the retraction was written.
--   event_seq    the seq of the result.retracted event on the check's issue, so a reader can
--                find the event this row was written by.
--
-- A table of its own rather than another check_results row. check_results refuses a row that
-- is not a whole result: its CHECK constraints hold role, commit_sha and verdict, so a
-- retraction stored there would have to carry a verdict of 'pass' or 'fail' that nobody
-- measured. loadCheckResults then hands every row of that table to the results listing and to
-- the Verification page, so that false verdict would have to be hidden again from every
-- reader.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"). The census answers 0, and rolling back past it is an image rollback alone.
create table check_result_retractions (
    result_id uuid primary key references check_results (id) on delete cascade,
    reason text not null check (reason <> ''),
    session text not null default '',
    actor jsonb not null,
    created_at timestamptz not null default now(),
    event_seq bigint
);
