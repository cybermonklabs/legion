-- The migration widens one check and does nothing else: it refuses no row, rewrites none and
-- deletes none. A wider check admits every row the narrower one admitted, so no stored value
-- changes its meaning. There is nothing here for a census to count.
select 0;
