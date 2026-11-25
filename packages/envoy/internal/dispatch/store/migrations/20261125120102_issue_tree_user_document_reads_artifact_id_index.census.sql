-- 20261125120102 builds one ordinary, non-unique index of user_document_reads concurrently: it
-- refuses and rewrites no row, and its build does not hold off user_document_reads' writers while
-- it scans the table, whatever the table's size.
select 0;
