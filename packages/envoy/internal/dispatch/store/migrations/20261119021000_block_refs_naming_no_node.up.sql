-- pgmigrate:non-transactional
-- 20261119021000_block_refs_naming_no_node.up.sql
-- envoy-dispatch backfill-graph-store walks the block references naming no node in batches, by
-- their key past the last batch's (graphcensus.BlockRefRowsStatement): `node_kind is null`, both
-- ends within the store's bound, `(from_node, kind, to_kind, to_id) > (...)`, in that order, 200 at
-- a time. No index served that walk: from_node is hash indexed (0121), which keeps no order, and
-- block_refs_node_address (20261111043100) serves `node_kind is null` but not the order, so each
-- batch read every row still naming no node and sorted them, and a run took time growing with the
-- square of the rows.
--
-- This index holds the rows the walk reads and keys them in its order, so each batch reads its own
-- 200 rows from the cursor on. Its predicate is the walk's own three clauses, as the walk writes
-- them, so they prove it. The bound is the store's address bound (graphstore.MaxAddressBytes, 1024
-- bytes) on each end, which also keeps every key inside what a B-tree entry holds: an address past
-- it, which 0121 hashes for that reason, is no row of this index. A row leaves the index when its
-- node is named, so after the backfill it holds only the rows a release before the store writes.
--
-- It is built `concurrently`, outside the runner's transaction: a plain build would hold SHARE on
-- block_refs, which blocks every write, for the length of a read of the table. A concurrent build
-- takes SHARE UPDATE EXCLUSIVE, which no ordinary read or write waits on, and the census exempts
-- it from its table size limit on that ground (pgmigrate's concurrentIndex), while no earlier
-- pending migration runs a DO block: it sorts before versions the release before it applied, so
-- that release's deployments hold it pending with this release's other backfill indexes alone
-- (store's TestCensusAdmitsTheBackfillIndexes). The columns it names come with 20261109022248,
-- which sorts before it. A failed build leaves an INVALID index of this name, which the runner
-- drops with DROP INDEX CONCURRENTLY and builds again on the next attempt. It is a performance
-- index: no constraint and no later migration depends on it.
create index concurrently if not exists block_refs_naming_no_node
    on block_refs (from_node, kind, to_kind, to_id)
    where node_kind is null and octet_length(from_node) <= 1024 and octet_length(to_id) <= 1024;
