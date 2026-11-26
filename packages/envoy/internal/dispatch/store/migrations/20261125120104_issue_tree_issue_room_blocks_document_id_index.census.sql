-- 20261125120104 builds one ordinary, non-unique index of issue_room_blocks concurrently: it
-- refuses and rewrites no row, and its build does not hold off issue_room_blocks' writers while it
-- scans the table, whatever the table's size.
select 0;
