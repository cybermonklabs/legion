-- 20261125120100 builds one ordinary, non-unique index of asks concurrently: it refuses and
-- rewrites no row, and its build does not hold off asks' writers while it scans the table,
-- whatever the table's size.
select 0;
