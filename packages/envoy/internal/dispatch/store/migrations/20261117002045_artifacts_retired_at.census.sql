-- The migration adds one nullable column with no default to artifacts and writes no row: every
-- stored row reads null in it, which is a document that stands, so it refuses no row and rewrites
-- no value any reader had before it. It cannot name the column here, since the column does not
-- exist when the census is taken. The number of existing rows this migration would refuse or
-- rewrite is therefore 0.
select 0
