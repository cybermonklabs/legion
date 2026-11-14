-- An ordinary, non-unique index refuses and rewrites no row. Its concurrent build does not hold
-- off ordinary writes while it scans document_blocks, whatever the table's size.
select 0;
