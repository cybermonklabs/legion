-- An ordinary, non-unique index refuses and rewrites no row. Its concurrent build does not hold
-- off ordinary writes to delivery_runs while it scans, whatever the table's size.
select 0;
