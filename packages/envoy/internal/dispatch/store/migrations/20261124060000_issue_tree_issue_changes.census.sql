-- 20261124060000 builds one ordinary, non-unique index of events concurrently: it refuses and
-- rewrites no row, and its build does not hold off events' writers while it scans the table,
-- whatever the table's size.
select 0;
