-- pgmigrate:non-transactional
-- 20261109022248 gave document_blocks a foreign key on (node_kind, node_address) and no index
-- over those columns. Postgres indexes the referenced side of a key and never the referencing
-- side, so every delete of a graph_nodes row reads this table once for each node deleted, with
-- nothing to read it by. Measured on this pod against 32,001 rows, deleting 16,809 nodes took
-- 2 minutes 51 seconds that way; with this index the same delete is seconds.
--
-- No statement of this tree deletes a graph_nodes row, so that delete is no path of this build.
-- The index also serves the graph_edges view's tuple arm, which probes a projection row by its
-- node (20261109215947), and document_blocks is the largest of the three tables it probes.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on document_blocks, which the runner
-- drops with DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists document_blocks_node_address
    on document_blocks (node_kind, node_address);
