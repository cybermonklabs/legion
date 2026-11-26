-- 20261125160000_issue_tree_node_search.up.sql — the search rows of the nodes that hold text.
--
-- node_search holds two kinds of search row (R17 of the issue-tree spec):
--
--   block     one row for each block node the store holds present (graph_nodes.state is not gone)
--             whose newest text value asserts (graphstore.AttributeText, the newest `text` tuple to
--             a value out of the node, by seq):
--               node_address  `<ref_key>#<block id>`, the room's tree artifact's ref_key for a
--                             block of an issue room and the document's own otherwise.
--               text          the value of that newest text tuple.
--               text_seq      that tuple's graph_tuples.seq.
--               search        search_vector('', search_document_text(text)), the vector a version's
--                             markdown takes (20261121060636's artifact_versions_search).
--
--   artifact  one row for each live document artifact (artifacts.kind = 'doc', retired_at is null,
--             and whose artifact node is not gone) that holds a version with markdown:
--               node_address  the document's ref_key.
--               text          empty: the row's vector is copied from the document's newest markdown
--                             version's search column (artifact_versions.search, built by
--                             artifact_versions_search() over the document's own fragment's
--                             markdown), and no reader reads text off an artifact row.
--               text_seq      that newest markdown version's number (artifact_versions.number).
--               search        that version's search vector, the exact vector 20261101182454's
--                             trigger wrote into document_search on main.
--
-- It takes the place of 20261101182447's document_search for this build's lexical search (R17):
-- search's document arm reads the block rows through node_search_vector and answers the block node
-- that matched; the related-item check's document arm, its lexeme probe and the near-copy gate read
-- the artifact rows through node_search_document_vector so they read one vector per document rather
-- than one per matched block. Every writer of a node or of a value out of a node writes the rows of
-- the block, artifact and version nodes it names in its own transaction, in the head write's batch
-- before the head rows (graphstore.QueueHeads, graphstore.QueueNodeSearch,
-- graphstore.QueueDocumentSearch), and the backfill writes those whose row stands behind
-- (graphcensus' backfillNodeSearch and backfillDocumentSearch). The census kinds node_search and
-- document_search_node hold each side to the store in both directions.
--
-- No key references graph_nodes: a key would take a lock of graph_nodes, which the pre-deploy census
-- would size as a table this migration touches, and no writer deletes a node.
--
-- Each GIN index is partial to its own node_kind so a search over blocks reads only block rows and
-- a related-item or duplicate check over documents reads only artifact rows (thousands, not tens of
-- thousands). node_search_ask_fences holds only block rows whose text opens a block of the type ask,
-- by the owner part of the address (the ref_key before the first '#'), and no other row. Its
-- predicate matches graphstore's NodeSearchAskNodesSQL, so the walk down from ask blocks takes the
-- index without scanning every other block or any document row.
--
-- A create table and three indexes on it, each exempt from the census's table limit
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
create index node_search_vector on node_search using gin (search)
  where node_kind = 'block';
create index node_search_document_vector on node_search using gin (search)
  where node_kind = 'artifact';
create index node_search_ask_fences on node_search (split_part(node_address, '#', 1))
  where node_kind = 'block'
    and case when left(text, 3) = ':::' then text ~ '^:{3,}ask\{' else false end;
