-- pgmigrate:non-transactional
-- The block-index tick marks again, one batch a tick, the user_threads rows a build before this
-- release leaves unmarked after the backfill (graphcensus.CatchUpOwedRows, through the backfill's
-- thread pass), on every tick that owes the object pass: every tick while another build serves,
-- the first after it left, and once every ten minutes in each process. Its read is the thread
-- pass's own, the rows whose marked_seq is null in (thread_id, login) order (20261116160000), and
-- no index held marked_seq, so each such tick read user_threads whole to find the few rows owed,
-- or none.
--
-- This index holds those rows alone, in the order the pass walks them: a row this release's
-- writer begins is marked from its insert, and the backfill marks every other, so the index holds
-- the rows an older build moved since and stays near empty. A tick that finds none owed reads one
-- index page. An update that marks a row takes it out; one that unmarks it, an older build's, puts
-- it in.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on user_threads, which the runner drops
-- with DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists user_threads_unmarked
    on user_threads (thread_id, login)
    where marked_seq is null;
