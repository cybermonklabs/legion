-- An ordinary, non-unique partial index refuses and rewrites no row. Its concurrent build does not
-- hold off ordinary reads or writes while it scans search_text_versions, whatever the table's size.
select 0;
