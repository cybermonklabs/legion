-- 20261126055316_signal_deadlines.up.sql — each project's one deadline of admission by signals, the
-- moment a signal with a limit would next cross it with no event.
--
--   - signal_deadlines is one row per project: due, the deadline (null for none: admission by
--     signals is off, no series has a limit, or none would cross its limit with no event), and
--     fence, which rises by one at each change of due. Admission writes it in the transaction of
--     each fact whose pass computed a deadline other than the one it holds (admit's
--     commitDeadline), and the daemon's one timer of the project fires at due once the fact
--     commits, applying intake.SignalDeadline under an event id that names the fence; inside the
--     fact lock a firing whose fence or due is not the row's is stale and runs no pass. The daemon
--     arms the timer from this row at each boot.
--
-- Every time is the daemon's clock's, never a column default. The migration adds one table and
-- changes none that an earlier release reads, so a daemon of the release before runs over the
-- migrated database, as a rollback leaves it: it arms no timer and reads no row of it.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon that shares the database stopped and before
-- the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. Dropping the
-- table and deleting this version row lets the roll-forward apply this migration anew: the first
-- pass after it computes the deadline again.
--
-- begin;
-- drop table if exists signal_deadlines;
-- delete from schema_version where version = 20261126055316;
-- commit;
create table signal_deadlines (
    project text        primary key check (project <> ''),
    due     timestamptz,
    fence   bigint      not null check (fence >= 1)
);
