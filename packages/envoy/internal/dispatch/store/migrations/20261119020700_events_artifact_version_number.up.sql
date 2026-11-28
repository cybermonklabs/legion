-- pgmigrate:non-transactional
-- 20261119020700_events_artifact_version_number.up.sql
-- envoy-dispatch backfill-graph-store reads, for each document and version of each batch of block
-- rows naming no node, the artifact.version event that recorded that version
-- (graphcensus.BlockRowsStatement): an event of that type whose payload names the document in
-- payload->>'artifact_id' and the version in payload->'version'->>'number', the newest such one.
-- No index served that read, so each one read the whole of events, which grows with every write:
-- a run read events once for every document version it backfilled, time that grows with the square
-- of the data. This index lets each read probe the document's version instead.
--
-- Its keys are the two expressions the read compares, as the read writes them, then id, so the
-- newest of the events that match is the first one read backwards. It holds the artifact.version
-- events alone, since the read asks for no other type: the read's own `type = 'artifact.version'`
-- proves the predicate.
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
create index concurrently if not exists events_artifact_version_number
    on events ((payload->>'artifact_id'), (payload->'version'->>'number'), id)
    where type = 'artifact.version';
