-- 20261126000200 re-states graph_edges, a catalogue write, and deletes the rows of
-- graph_edges_retirements, a table only the boot step writes, which holds one row per child at
-- most: it reads no row of a projection, and refuses and rewrites none. Each part it folds into
-- the tuple arm keeps the tuples its arm kept, so every edge the view answered before it is
-- answered as it was.
select 0;
