-- One index dropped and a lock bound set: the drop reads no row of block_refs, refuses none and
-- rewrites none. The census report still names block_refs, the table behind the index, so a
-- deployment sees what the drop locks.
select 0;
