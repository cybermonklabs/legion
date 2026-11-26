-- 20261126050009 only re-states graph_edges, a catalogue write: it reads no stored row, and
-- refuses and rewrites none. Its tuple arm tests a pointer's target through block_node_listed,
-- which holds block_ref_listed's body until a boot step restates it, so every edge the view
-- answered it answers as it did. The number of existing rows this migration would refuse or
-- rewrite is therefore 0.
select 0;
