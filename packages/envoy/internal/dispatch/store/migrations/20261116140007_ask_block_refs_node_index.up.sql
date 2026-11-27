-- pgmigrate:non-transactional
-- 20261116140000 gave ask_block_refs a foreign key on (node_kind, node_address) and no index over
-- those columns, and Postgres indexes the referenced side of a key and never the referencing side.
-- So every delete of a graph_nodes row would read this table once for each node deleted, with
-- nothing to read it by, as 20261116140002 says of comment_blocks.
--
-- The same index serves two reads of this release. graph_edges' arm over an ask's parts, and
-- refs.BacklinkCounts beside it, answer a part's link only while the ask_block_refs row of that
-- link names the part's node (migration 20261126045003), one probe of this index per edge, which
-- the link's kind and far end narrow to its one row. And a btree holds the rows whose columns are
-- null, so the walk over the rows that name no node, the rows the old arm still answers, reads
-- them through it rather than through every row of ask_block_refs.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on ask_block_refs, which the runner drops
-- with DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists ask_block_refs_node_address
    on ask_block_refs (node_kind, node_address);
