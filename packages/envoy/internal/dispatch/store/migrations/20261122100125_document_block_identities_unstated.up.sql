-- pgmigrate:non-transactional
-- The backfill and the block-index tick's catch-up read the documents a build before this release
-- stated the blocks of without their identity mentions (graphcensus' blockIdentitiesPass): the
-- document_block_versions rows whose identities_stated is false (20261122100124), in artifact_id
-- order, on every tick that owes the catch-up, and that tick's own probe of whether any is owed
-- (graphcensus.EventsOweObjectPass). The primary key holds every document, so each such tick read
-- the table whole to find the few rows owed, or none.
--
-- This index holds those rows alone, in the order the pass walks them: every row this release
-- inserts holds true, the catch-up sets true on each row it reads, and only a write of a build
-- before this release sets false, so after the first backfill the index holds the documents such
-- a build wrote since and stays near empty. A tick that finds none owed reads one index page.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on document_block_versions, which the
-- runner drops with DROP INDEX CONCURRENTLY and builds again on the next attempt.
-- kept for one release: block-identities-stated
create index concurrently if not exists document_block_versions_identities_unstated
    on document_block_versions (artifact_id)
    where not identities_stated;
