-- pgmigrate:non-transactional
-- A window's id is <owner key>#<first block>. The indexer's per-owner delete filters by kind
-- and split_part(id, '#', 1), so this expression index avoids scanning unrelated owners.
-- It is a performance index: no constraint or later migration depends on it. The four trigger
-- drops after 20261102233625 keep their recorded versions, so the first free version is used.
-- Databases that already applied the former index build in 20261102233625 keep their valid
-- index (including its window-kind predicate). A failed concurrent build leaves an INVALID
-- index, which the runner refuses with DROP INDEX CONCURRENTLY recovery instructions.
create index concurrently if not exists embeddings_window_owner on embeddings (kind, split_part(id, '#', 1));
