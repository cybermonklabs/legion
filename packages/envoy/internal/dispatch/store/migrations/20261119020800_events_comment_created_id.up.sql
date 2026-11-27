-- pgmigrate:non-transactional
-- 20261119020800_events_comment_created_id.up.sql
-- envoy-dispatch backfill-graph-store reads, for each comment of each batch of comments naming no
-- node, the comment.created event that stored it (graphcensus.CommentRowsStatement): an event of
-- that type whose payload names the comment in payload->>'id', the first such one. No index served
-- that read, so each one read the whole of events, which grows with every write: a run read events
-- once for every comment it backfilled, time that grows with the square of the data. This index
-- lets each read probe the comment instead.
--
-- Its keys are the expression the read compares, as the read writes it, then id, so the first of
-- the events that match is the first one read. It holds the comment.created events alone, since
-- the read asks for no other type: the read's own `type = 'comment.created'` proves the predicate.
--
-- It is built `concurrently`, outside the runner's transaction: a plain build would hold SHARE on
-- events, which blocks every write, for the length of a read of the table. A concurrent build
-- takes SHARE UPDATE EXCLUSIVE, which no ordinary read or write waits on, and the census exempts
-- it from its table size limit on that ground (pgmigrate's concurrentIndex), while no earlier
-- pending migration runs a DO block: it sorts before versions the release before it applied, so
-- that release's deployments hold it pending with this release's other backfill indexes alone
-- (store's TestCensusAdmitsTheBackfillIndexes). A failed build leaves an INVALID index of this
-- name, which the runner drops with DROP INDEX CONCURRENTLY and builds again on the next attempt.
-- It is a performance index: no constraint and no later migration depends on it.
create index concurrently if not exists events_comment_created_id
    on events ((payload->>'id'), id)
    where type = 'comment.created';
