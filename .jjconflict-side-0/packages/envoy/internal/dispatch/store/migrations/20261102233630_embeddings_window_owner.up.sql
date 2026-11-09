-- pgmigrate:non-transactional
-- A window's id is <owner key>#<first block>. The indexer's per-owner delete filters by kind
-- and split_part(id, '#', 1), so this expression index avoids scanning unrelated owners.
-- Keep the window-kind predicate: it preserves ordered owner walks through this index and
-- keeps the whole-text retirement reads on their primary key rather than this expression index.
-- It is a performance index: no constraint or later migration depends on it. The four trigger
-- drops after 20261102233625 keep their recorded versions, so the first free version is used.
-- Databases that already applied the former index build in 20261102233625 keep their valid
-- index (including its window-kind predicate). A failed concurrent build leaves an INVALID
-- index of this name on embeddings, which the runner drops with DROP INDEX CONCURRENTLY and
-- builds again on the next attempt.
create index concurrently if not exists embeddings_window_owner
    on embeddings (kind, split_part(id, '#', 1))
    where kind in ('document_block', 'comment_block', 'message_block', 'ask_part');
