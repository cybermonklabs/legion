-- The migration adds a nullable column to the three item state tables, replaces the trigger
-- function that owes an item its block index so that it owes the window arm too, and builds a
-- partial index on each table over a column that is null in every row, so each index is built
-- empty. It refuses no row and rewrites no value any reader had before it: the column is new and
-- no earlier release names it. It cannot name the column, which does not exist when the census is
-- taken; the rows its backfill marks are 20261107203841's, and new work owed rather than a
-- rewrite, as 0143's seed is.
select 0
