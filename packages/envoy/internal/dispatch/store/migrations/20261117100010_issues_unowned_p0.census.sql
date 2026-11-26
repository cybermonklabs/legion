-- An ordinary, non-unique index refuses and rewrites no row. Its concurrent build does not hold
-- off ordinary writes to issues while it scans, whatever the table's size.
select 0;
