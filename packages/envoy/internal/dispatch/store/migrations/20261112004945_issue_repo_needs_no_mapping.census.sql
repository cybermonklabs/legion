-- The migration drops two constraints and does nothing else: it refuses no row, rewrites none
-- and deletes none. Dropping a rule admits more rows than before and never fewer, so every row
-- issues_repo_mapped_to_project admitted the schema still admits, and no stored value changes
-- its meaning. There is nothing here for a census to count.
select 0;
