-- pgmigrate:non-transactional
-- The block-index tick runs the backfill's citation passes, one batch a tick, over the refs rows
-- out of a comment, a message or an ask a build before this release wrote after the backfill
-- (graphcensus.CatchUpOwedRows), on every tick that owes the object pass. Each reads the rows of its
-- item kind whose tuples_stored is false (20261116135959) in key order, and the primary key held
-- every row of the kind, so each such tick read every citation of each kind to find the few owed.
--
-- This index holds those rows alone: every citation this release writes holds tuples_stored from
-- its insert and the backfill sets it on every other the store can state, so the index holds the
-- citations past the store's bound and those an older build wrote since. A document's rows are
-- none of it: no writer of this release states a document's citation as a tuple, so every one of
-- them holds false, and an index over them would hold most of refs.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on refs, which the runner drops with DROP
-- INDEX CONCURRENTLY and builds again on the next attempt.
-- kept for one release: inbox-roll-catch-up
create index concurrently if not exists refs_item_tuples_unstored
    on refs (from_kind, from_id, to_kind, to_id)
    where not tuples_stored and from_kind in ('comment', 'message', 'ask');
