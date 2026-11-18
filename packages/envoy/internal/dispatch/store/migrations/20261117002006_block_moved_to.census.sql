-- The migration creates one function, block_moved_to, and nothing else: it writes no row, reads no
-- row while it applies (plpgsql validates the body without running it), and changes no table, so
-- it refuses no row and rewrites no value any reader had before it. The number of existing rows
-- this migration would refuse or rewrite is therefore 0.
select 0
