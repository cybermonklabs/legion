-- pgmigrate:non-transactional
-- 20261116172615_graph_nodes_proposed_index.up.sql — the index the read of an issue's proposed
-- nodes takes.
--
-- This release gives the node state the value `proposed`, which a node holds while it is
-- a question still open, and GET /api/v1/issues/{key}/proposed answers the nodes under one issue
-- at that value (api/node_state.go, issueProposedQuery). A node of any kind may be proposed, and
-- graph_nodes holds a row for every block of every document, so without this index that read is a
-- scan of the whole table for the few rows at `proposed`. The index holds those rows alone: it is
-- partial on `state = 'proposed'`, the literal the read binds, and keyed on the primary key's
-- columns, so a row moves into it when a write marks the node proposed and out when the node is
-- settled or moved on.
--
-- It is a performance index: no constraint and no later migration depends on it, and a build of
-- the release before reads and writes graph_nodes as it did. A failed concurrent build leaves an
-- INVALID index of this name, which the runner drops with DROP INDEX CONCURRENTLY and builds again
-- on the next attempt.
create index concurrently if not exists graph_nodes_proposed
    on graph_nodes (kind, id) where state = 'proposed';
