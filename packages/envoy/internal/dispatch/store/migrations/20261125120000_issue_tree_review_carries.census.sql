-- The migration creates a new, empty table with no reference to an existing table.
-- No existing row is refused or rewritten; the graph backfill runs separately.
select 0
