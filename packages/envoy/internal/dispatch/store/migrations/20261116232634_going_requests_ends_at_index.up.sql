-- pgmigrate:non-transactional
-- 20261116232634_going_requests_ends_at_index.up.sql — the index the sweep of overdue runs reads.
--
-- dispatch://INFRA-1950 keeps every run of the command bar, its going_requests row, its frames and
-- its interaction node, until an erase removes it, where the release before deleted a run an hour
-- old. So going_requests grows with every question asked, and the maintenance pass reads it once a
-- minute for the runs past their ends_at that hold no terminal frame (api/going.go,
-- sweepGoingRequests). That read takes only the runs whose ends_at passed within a day of now
-- (api.goingSweepWindow, goingOverdueRuns), and this index is what makes that bound a bound:
-- without it the read is a scan of every run ever kept, each minute. Keyed on ends_at alone, it
-- serves the range both bounds give and the read's order by ends_at.
--
-- It is a performance index: no constraint and no later migration depends on it, and a build of
-- the release before reads and writes going_requests as it did. It is built concurrently, in a
-- migration of its own, because going_requests holds rows by now and a plain build would hold off
-- every POST of the command bar while it scans them. A failed concurrent build leaves an INVALID
-- index of this name, which the runner drops with DROP INDEX CONCURRENTLY and builds again on the
-- next attempt.
create index concurrently if not exists going_requests_ends_at
    on going_requests (ends_at);
