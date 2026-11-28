-- 20261125120101 builds one ordinary, non-unique index of related_checks concurrently: it refuses
-- and rewrites no row, and its build does not hold off related_checks' writers while it scans the
-- table, whatever the table's size.
select 0;
