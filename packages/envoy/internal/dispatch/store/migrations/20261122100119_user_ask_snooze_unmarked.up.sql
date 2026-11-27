-- pgmigrate:non-transactional
-- The block-index tick runs the backfill's read-state pass over user_ask_snooze, one batch a tick,
-- over the rows a build before this release wrote after the backfill (graphcensus.CatchUpOwedRows),
-- on every tick that owes the object pass. The pass reads the rows whose tuples_stored_at is null
-- (20261116170000) in (login, ask_id) order, and no index held that column, so each such tick read
-- user_ask_snooze whole to find the few rows owed, or none.
--
-- This index holds those rows alone, in the order the pass walks them: every row this release
-- writes holds tuples_stored_at from its statement and the backfill sets it on every other the
-- store can key, so the index holds the rows past the store's bound and those an older build wrote
-- since.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on user_ask_snooze, which the runner drops
-- with DROP INDEX CONCURRENTLY and builds again on the next attempt.
-- kept for one release: inbox-roll-catch-up
create index concurrently if not exists user_ask_snooze_unmarked
    on user_ask_snooze (login, ask_id)
    where tuples_stored_at is null;
