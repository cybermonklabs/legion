-- An ordinary, non-unique partial index refuses and rewrites no row. Its concurrent build does not
-- hold off ordinary reads or writes while it scans user_threads, whatever the table's size.
select 0;
