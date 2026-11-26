-- 20261125160000_issue_tree_node_search.up.sql — the search rows of the nodes that hold text.
--
-- node_search holds one row for each block node the store holds present (graph_nodes.state is not
-- gone) whose newest text value asserts (graphstore.AttributeText, the newest `text` tuple to a
-- value out of the node, by seq):
--
--   node_kind     the node's kind, block.
--   node_address  the node's id, `<ref_key>#<block id>`: for a block of an issue room the room's
--                 tree artifact's ref_key, for a block of a document that is its own room the
--                 document's.
--   text          the value of that newest text tuple.
--   text_seq      that tuple's graph_tuples.seq. A write of the row replaces one of a lower seq
--                 alone, and its delete takes one of a seq no higher than the tuple it read, so a
--                 write that read the store before another committed leaves the other's row.
--   search        search_vector('', search_document_text(text)), the vector a version's markdown
--                 takes (20261121060636's artifact_versions_search), over the node's text.
--
-- It takes the place of 20261101182447's document_search for this build's lexical search (R17):
-- search's document arm, the related-item check's probe and the near-copy gate's scoring each find
-- a word through node_search_vector and answer the node that holds it. Every writer of a node or of
-- a value out of a node writes the rows of the block nodes it names in its own transaction, in the
-- head write's batch before the head rows (graphstore.QueueHeads, graphstore.QueueNodeSearch), and
-- the backfill writes those of the nodes whose row stands behind them (graphcensus'
-- backfillNodeSearch). The census kind node_search holds the table to the store in both directions.
--
-- No key references graph_nodes: a key would take a lock of graph_nodes, which the pre-deploy census
-- would size as a table this migration touches, and no writer deletes a node.
--
-- A create table and an index on it, each exempt from the census's table limit
-- (internal/pgmigrate/exempt.go: catalogOnlyCreateTable, and catalogOnlyIndex over a table the same
-- migration creates). It names no other table: artifact_versions, document_search and its trigger
-- stay as they are for the root's own pull request to drop (R17, R19).
--
-- Rolling deploy. The table is new and holds no row; the release before names it nowhere and writes
-- no text value at all. Rolling back past this migration is an image rollback alone: the release
-- before searches document_search, which 20261101182454's trigger keeps filled, and reads nothing of
-- this table.
create table node_search (
  node_kind text not null,
  node_address text not null,
  text text not null,
  text_seq bigint not null,
  search tsvector not null,
  primary key (node_kind, node_address)
);
create index node_search_vector on node_search using gin (search);
