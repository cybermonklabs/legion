-- The migration creates two functions, block_address_walk and block_room_address, and replaces the
-- bodies of two more, moved_to_reach and block_moved_to, and nothing else: it writes no row, reads
-- no row while it applies (plpgsql validates a body without running it, and a sql function's body
-- is parsed, not run), and changes no table, so it refuses no row and rewrites no value any reader
-- had before it. Every call the release before makes answers what it answered before (the
-- migration's head says why). The number of existing rows this migration would refuse or rewrite
-- is therefore 0.
select 0
