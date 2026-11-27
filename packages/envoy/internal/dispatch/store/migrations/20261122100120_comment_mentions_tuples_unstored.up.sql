-- pgmigrate:non-transactional
-- The block-index tick runs the backfill's mention pass, one batch a tick, over the
-- comment_mentions rows a build before this release wrote after the backfill
-- (graphcensus.CatchUpOwedRows), on every tick that owes the object pass. Its read is the pass's
-- own, the rows whose tuples_stored is false (20261120090000) in (comment_id, target) order, and
-- the primary key held every row, so each such tick read comment_mentions whole to find the few
-- rows owed, or none.
--
-- This index holds those rows alone, in the order the pass walks them: every row this release's
-- comment write stores holds tuples_stored from its insert, and the backfill sets it on every
-- other the store can state, so the index holds the rows past the store's bound and those an
-- older build wrote since, and stays near empty. A tick that finds none owed reads one index page.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on comment_mentions, which the runner
-- drops with DROP INDEX CONCURRENTLY and builds again on the next attempt.
-- kept for one release: inbox-roll-catch-up
create index concurrently if not exists comment_mentions_tuples_unstored
    on comment_mentions (comment_id, target)
    where not tuples_stored;
