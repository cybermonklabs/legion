-- The migration creates block_node_listed, a new function, and nothing else: it writes no row,
-- reads none and alters no table. The number of existing rows this migration would refuse or
-- rewrite is therefore 0.
select 0;
