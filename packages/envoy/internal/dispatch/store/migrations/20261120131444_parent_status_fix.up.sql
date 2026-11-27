-- 20261120131444_parent_status_fix.up.sql
--
-- INFRA-4731: the record that R25's fix ran on this database. With DISPATCH_PARENT_STATUS_RULE
-- on, one process runs the fix at boot under the lease parent-status-fix
-- (api.RunParentStatusFixAtBoot): it unlinks each open issue from its done parent, then moves
-- each not-started ancestor of an issue in work to in_progress. It writes this row once the last
-- change has committed, and every process serves only once the row is there. A later boot that
-- finds the row runs nothing, also after the setting was off for a time, so the fix runs once
-- per database.
--
--   id         always true, which the primary key and the check hold the table to: one row at
--              most.
--   ran_at     when the fix wrote the record, by the database's clock.
--   unlinked   how many issues the fix unlinked from their done parent.
--   moved      how many issues the fix moved to in_progress.
--
-- A new table nothing before this release names: the release before this one neither reads nor
-- writes it, so the two roll side by side (cmd/dispatch/README.md, "Database migrations"), and
-- the census answers 0 because the table holds no row at this schema. Rolling back past it is an
-- image rollback alone. The identifier is the UTC moment this file was created, as every
-- migration past the numbered series is named.
create table parent_status_fix (
    id boolean primary key default true check (id),
    ran_at timestamptz not null default now(),
    unlinked integer not null check (unlinked >= 0),
    moved integer not null check (moved >= 0)
);
