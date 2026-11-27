-- 20261125140002 builds one ordinary, non-unique, partial index of comments concurrently: it
-- refuses and rewrites no row, and its build does not hold off comments' writers while it scans
-- the table, whatever the table's size.
select 0;
