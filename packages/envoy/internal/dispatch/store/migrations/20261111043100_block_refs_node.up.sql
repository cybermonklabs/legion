-- pgmigrate:non-transactional
-- 20261109022248 gave block_refs a foreign key on (node_kind, node_address) and no index over
-- those columns, and Postgres indexes the referenced side of a key and never the referencing
-- side. So every delete of a graph_nodes row reads this table once for each node deleted, with
-- nothing to read it by.
--
-- The sibling index on document_blocks (20261111043000) carries the measurement: deleting
-- 16,809 nodes with no such index read that table for each one and took 2 minutes 51 seconds.
-- A node is deleted by the erase (envoy-dispatch erase), which removes a node and the rows
-- that project it, and by a projection's own retirement.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on block_refs, which the runner drops
-- with DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists block_refs_node_address
    on block_refs (node_kind, node_address);
