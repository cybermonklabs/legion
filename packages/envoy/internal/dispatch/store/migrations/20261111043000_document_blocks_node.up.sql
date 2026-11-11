-- pgmigrate:non-transactional
-- 20261109022248 gave document_blocks a foreign key on (node_kind, node_address) and no index
-- over those columns. Postgres indexes the referenced side of a key and never the referencing
-- side, so every delete of a graph_nodes row reads this table once for each node deleted, with
-- nothing to read it by. Measured on this pod against 32,001 rows, deleting 16,809 nodes took
-- 2 minutes 51 seconds that way; with this index the same delete is seconds.
--
-- A node is deleted on two paths this tree owns: the erase (envoy-dispatch erase), which
-- removes a node and the rows that project it, and a projection's own retirement. Both run
-- against the production tables, where document_blocks is the largest of the three, so the
-- scan is not a test-only cost.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on document_blocks, which the runner
-- drops with DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists document_blocks_node_address
    on document_blocks (node_kind, node_address);
