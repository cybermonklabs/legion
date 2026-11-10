-- pgmigrate:non-transactional
-- The ?artifact= narrowing of GET /api/v1/comment-threads/by-target and
-- GET /api/v1/comment-threads/by-thread-state reads the threads anchored in one document, oldest
-- first by (created_at, id), which is also the order those routes page in.
--
-- comments_anchor_artifact (0032) does not serve it. That index is on
-- ((anchor->>'artifact_id')::uuid), and the routes compare the stored text against a caller's
-- value, which is a text comparison an expression index on the uuid cast cannot answer; casting
-- the caller's value instead would raise 22P02 on anything that is not a uuid. It also carries no
-- order columns, so a keyset page through it would sort every matching row.
--
-- This index carries the comparison the routes make and the two columns they order by, so one
-- page is one index range read however many threads the document holds. It is a performance
-- index: no constraint or later migration depends on it.
--
-- Built concurrently because comments is a large production table (464,602 rows on the load
-- test's own corpus, dispatch://INFRA-988). A failed concurrent build leaves an INVALID index of
-- this name, which the runner drops with DROP INDEX CONCURRENTLY and builds again on the next
-- attempt; an INVALID index is never used and announces nothing, so the package's plan test
-- asserts pg_index.indisvalid for this index rather than its existence.
create index concurrently if not exists comments_anchor_artifact_created
    on comments ((anchor->>'artifact_id'), created_at, id)
    where anchor->>'artifact_id' is not null;
