-- 20261120220100_issue_tree_version_render_digest.up.sql — the digest of a version rendered from
-- the tree is its record's.
--
-- artifact_version_text_sha256 is the digest an approval of a version pins: the insert trigger
-- artifact_reviews_content_sha256_insert sets artifact_reviews.content_sha256 from it whatever the
-- insert supplies, and the update trigger refuses any statement that would move a pinned one
-- (20261105084017_artifact_review_content_sha256). A version the tree renders holds a
-- version_renders row (20261120220000) whose record_sha256 is the digest of the record of its
-- document's query, which the server builds in Go (docs.IssueTree.QueryRecord) and no SQL could
-- build again; every other version, each one written before the tree was rendered from among them,
-- holds none. This restates the function alone, as a create or replace of the same signature: it
-- answers the row's record_sha256 where a row stands, and else, as before, the sha256 of the
-- version's stored markdown, which is the digest every review standing today already holds. Both
-- triggers keep their text, so a pinned digest still never moves.
--
-- Rolling deploy. The release before names no version_renders row and writes none, so for every
-- version it reads or approves the function answers what it answered before. Rolling back past
-- this migration is an image rollback alone; the function then answers the record's digest of a
-- version this release rendered, which no build before this one writes.
create or replace function artifact_version_text_sha256(artifact uuid, version integer)
returns text language sql stable parallel safe as $$
    select coalesce(r.record_sha256, encode(sha256(convert_to(v.markdown, 'UTF8')), 'hex'))
    from artifact_versions v
    left join version_renders r on r.artifact_id = v.artifact_id and r.number = v.number
    where v.artifact_id = $1 and v.number = $2
$$;
