-- 20261109022248_graph_projections.up.sql
--
-- The three projected tables learn the address of the node they project. A node is a thing
-- Dispatch holds and a tuple is one fact stated about it (20261109013400_graph_store.up.sql);
-- document_blocks, block_refs and comments stay where they are and become projections of those
-- tuples, so each row has to say which node it is the projection of.
--   node_kind      the `kind` of the row's node in graph_nodes.
--   node_address   its `id` there. Two columns rather than one text address, because that is how
--                  graph_nodes is keyed and a foreign key references a key as it is written.
--   parent_block_id  document_blocks' alone: the block of the same document that holds this one,
--                  null for a block at the document's top level. A block_refs row is one link a
--                  block holds and a comment is no block, so neither nests and neither takes it.
--
-- Every column is nullable and nothing fills any of them in this commit: the writers that fill
-- them come with the cutover of each kind (graph_store_cutover). A composite foreign key is
-- MATCH SIMPLE, which checks no row where any of its columns is null, so every row these tables
-- already hold passes the new constraint and the census answers 0.
--
-- The foreign key is not deferrable, which is the default: Postgres checks it inside the
-- statement that writes the projection row, so a write naming a node the store does not hold
-- fails there, at the writer, and no deferred trigger queues work for COMMIT to run through.
-- That matters because a projection is written row by row on the hot write path, and a deferred
-- check would hold every such row's work until the transaction ends.
alter table document_blocks add column node_kind text, add column node_address text,
  add column parent_block_id text,
  add constraint document_blocks_node foreign key (node_kind, node_address) references graph_nodes (kind, id);

alter table block_refs add column node_kind text, add column node_address text,
  add constraint block_refs_node foreign key (node_kind, node_address) references graph_nodes (kind, id);

alter table comments add column node_kind text, add column node_address text,
  add constraint comments_node foreign key (node_kind, node_address) references graph_nodes (kind, id);
