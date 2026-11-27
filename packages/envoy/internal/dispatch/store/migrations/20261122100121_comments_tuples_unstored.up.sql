-- pgmigrate:non-transactional
-- The block-index tick runs two of the backfill's comment passes, one batch a tick, over the
-- comments rows a build before this release wrote after the backfill (graphcensus.CatchUpOwedRows),
-- on every tick that owes the object pass: the pointer pass over the comments whose tuples_stored is
-- false and which name their node, as a build that writes the comment's node itself writes them,
-- and the past-bound pass over those the store keys no pointer of. Each reads in id order the rows
-- whose tuples_stored is false (20261116124447), and the primary key held every row, so each such
-- tick read comments whole to find the few rows owed, or none.
--
-- This index holds those rows alone, in the order both passes walk them: every comment this
-- release writes holds tuples_stored from its insert where the store keys its pointers, and the
-- backfill sets it on every other the store can state, so the index holds the comments past the
-- store's bound and those an older build wrote since, and stays near empty.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on comments, which the runner drops with
-- DROP INDEX CONCURRENTLY and builds again on the next attempt.
-- kept for one release: inbox-roll-catch-up
create index concurrently if not exists comments_tuples_unstored
    on comments (id)
    where not tuples_stored;
