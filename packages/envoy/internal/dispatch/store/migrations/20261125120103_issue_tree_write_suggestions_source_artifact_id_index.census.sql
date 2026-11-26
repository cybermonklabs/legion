-- 20261125120103 builds one ordinary, non-unique index of write_suggestions concurrently: it
-- refuses and rewrites no row, and its build does not hold off write_suggestions' writers while it
-- scans the table, whatever the table's size.
select 0;
