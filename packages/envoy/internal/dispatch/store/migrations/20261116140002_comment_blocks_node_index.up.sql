-- pgmigrate:non-transactional
-- 20261116140000 gave comment_blocks a foreign key on (node_kind, node_address) and no index over
-- those columns, and Postgres indexes the referenced side of a key and never the referencing side.
-- So every delete of a graph_nodes row would read this table once for each node deleted, with
-- nothing to read it by, as 20261111043200 says of comments, whose sibling index on
-- document_blocks (20261111043000) carries the measurement.
--
-- The same index serves two reads of this release. graph_edges' arm over a comment's blocks, and
-- refs.BacklinkCounts beside it, answer a block's part_of only while a comment_blocks row names
-- the block's node (migration 20261126045003), one probe of this index per edge. And a btree holds
-- the rows whose columns are null, so the walk over the rows that name no node, the rows the old
-- arm still answers, reads them through it rather than through every row of comment_blocks.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on comment_blocks, which the runner drops
-- with DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists comment_blocks_node_address
    on comment_blocks (node_kind, node_address);
