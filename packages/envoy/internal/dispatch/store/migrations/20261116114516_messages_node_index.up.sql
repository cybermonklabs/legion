-- pgmigrate:non-transactional
-- 20261116114515 gave messages a foreign key on (node_kind, node_address) and no index over those
-- columns, and Postgres indexes the referenced side of a key and never the referencing side. So
-- every delete of a graph_nodes row would read this table once for each node deleted, with nothing
-- to read it by, as 20261111043200 says of comments, whose sibling index on document_blocks
-- (20261111043000) carries the measurement.
--
-- The same index serves two reads of this release. graph_edges' tuple arm, and refs.BacklinkCounts
-- beside it, answer a message's replies_to only while a messages row names the message's node
-- (migration 20261116114517), one probe of this index per edge. And a btree holds the rows whose
-- columns are null, so the walk over the rows that name no node, the rows the old arms still
-- answer, reads them through it rather than through every row of messages.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on messages, which the runner drops with
-- DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists messages_node_address
    on messages (node_kind, node_address);
