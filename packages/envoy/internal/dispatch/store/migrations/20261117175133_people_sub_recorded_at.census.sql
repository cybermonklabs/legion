-- 20261117175133 adds people.sub_recorded_at as a nullable timestamptz with no default, a
-- catalog-only alter table, and two plpgsql functions and three triggers (the stamp, the kept row
-- on delete, the refused truncate) written into the catalog alone; the triggers run on later
-- writes only, so the migration reads, refuses and rewrites no row, and counts 0.
select 0;
