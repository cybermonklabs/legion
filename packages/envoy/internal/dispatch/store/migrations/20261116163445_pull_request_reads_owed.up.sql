-- 20261116163445_pull_request_reads_owed.up.sql
--
-- The pull requests whose read of GitHub has not yet answered for a change Dispatch knows of,
-- held until a read of GitHub answers for them.
--
--   repo, number  the pull request, as pull_request_heads names one: the repository under its
--                 canonical name (refs.CanonicalRepo) and the number.
--   reason        why the read is owed, as the last read of it answered: 'failure' where GitHub
--                 did not answer (a 5xx, a timeout, a rate limit, a head write that failed), or
--                 where a write no read of GitHub has the last word on owes it (the triggers of
--                 20261116182209); 'refusal' where GitHub answered that it cannot give the pull
--                 request (a 404 or 410, or a repository the App cannot read). A read that
--                 answers the other way changes it.
--   reads         how many times the reconcile has read it; the intake's own read is not one.
--   owed_at       when the read was last owed; recording it again moves it and keeps reads.
--                 The order a pass reads the rows in: a schedule, never the proof of what a
--                 read covers, and never the end of a read.
--   revision      a value of the column's own sequence, new at each owe (an insert, or an update
--                 of the row by another owe). A read takes it before it reads GitHub and deletes
--                 the row only while it still has that value (delivery.settleOwedRead), so an
--                 owe made after the read began, by any transaction and whatever any clock says,
--                 stays. A sequence and not a count of the row: a row deleted and owed again
--                 never gets back a revision a read took before.
--
-- R2 makes GitHub's answer the only source of a pull request's head, so an event whose read of
-- GitHub failed would leave its pull request with no head: the reconcile reads only the
-- pull_request_heads rows that exist, never reads a merged or closed row again, and the
-- read-through runs only for a result's commit; and a read made a moment after GitHub's own
-- webhook can get GitHub's 404. So the intake records the pull request here before it
-- acknowledges such an envelope, and the reconcile's head pass reads each one from GitHub until
-- GitHub answers, or until GitHub's 404 comes from a repository the App can read
-- (delivery.Reconcile.readOwedPullRequests). A row holds no head, no state and no issue, and no
-- reader of a head reads this table, so an owed read is never a head, a basis or a merge.
--
-- The two reasons have a budget each in a pass, and every failure is read before any refusal:
-- any bearer of Envoy's /v1 publish route can name a pull request GitHub never had, and each such
-- envelope owes a refusal, so refusals must take no read from a failure. Inside each reason the
-- row read the fewest times goes first, then the
-- one owed longest, so every row has its first read before any row has its second. The index
-- serves that order, so a table of forged refusals costs a pass an index scan of its budget and
-- not a sort of every row.
--
-- A new table and the sequence of its revision, which name no other: the transaction locks only
-- them, which no other session can reach before the commit, and nothing existing is read, locked
-- or rewritten. The release before this one neither reads nor writes them, so it rolls beside this
-- one (cmd/dispatch/README.md, "Database migrations"). The census answers 0. Rolling back past it
-- is an image rollback alone; the reads still owed then wait for the next event of each pull
-- request, as they did before this release.
create table pull_request_reads_owed (
    repo text not null,
    number integer not null,
    reason text not null check (reason in ('failure', 'refusal')),
    reads integer not null default 0,
    owed_at timestamptz not null default now(),
    revision bigserial,
    primary key (repo, number)
);

create index pull_request_reads_owed_order on pull_request_reads_owed (reason, reads, owed_at, repo, number);
