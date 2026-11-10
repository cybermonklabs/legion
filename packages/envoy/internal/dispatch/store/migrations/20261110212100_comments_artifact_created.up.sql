-- pgmigrate:non-transactional
-- The project scope of GET /api/v1/comment-threads/by-target and
-- GET /api/v1/comment-threads/by-thread-state reads two sets of comments: those of the project's
-- issues, which comments_issue_created (0040) already serves in the routes' own order, and those
-- of the project's unlinked documents, which carry an artifact_id and no issue key (owner.go).
--
-- comments_artifact_id (0032) answers the equality but carries no order columns, so a keyset page
-- through it would read every comment of the document and sort them. This index carries
-- artifact_id with the two columns the routes order and page by, so that arm is one index range
-- read like the issue arm beside it. It is a performance index: no constraint or later migration
-- depends on it, and comments_artifact_id stays, since graph_edges and the document reads lead
-- with artifact_id alone.
--
-- Built concurrently because comments is a large production table (464,602 rows on the load
-- test's own corpus, dispatch://INFRA-988). A failed concurrent build leaves an INVALID index of
-- this name, which the runner drops with DROP INDEX CONCURRENTLY and builds again on the next
-- attempt; an INVALID index is never used and announces nothing, so the package's plan test
-- asserts pg_index.indisvalid for this index rather than its existence.
create index concurrently if not exists comments_artifact_created
    on comments (artifact_id, created_at, id)
    where artifact_id is not null;
