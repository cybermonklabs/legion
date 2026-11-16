-- 20261116163445_pull_request_reads_owed.up.sql
--
-- The pull requests whose read of GitHub has not yet answered for a change Dispatch knows of,
-- held until a read of GitHub answers for them (dispatch://INFRA-3992, R1 to R4).
--
--   repo, number  the pull request, as pull_request_heads names one: the repository under its
--                 canonical name (refs.CanonicalRepo) and the number.
--   reason        why the read is owed, as the last read of it answered: 'failure' where GitHub
--                 did not answer (a 5xx, a timeout, a rate limit, the intake's delivery cap), or
--                 where a read of this build wrote an answer older than the row it replaced;
--                 'refusal' where GitHub answered that it cannot give the pull request (a 404 or
--                 410, or a repository the App cannot read). A read that answers the other way
--                 changes it.
--   reads         how many times the reconcile has read it; the intake's own read is not one.
--   owed_at       when the read was last owed; recording it again moves it and keeps reads.
--
-- R2 makes GitHub's answer the only source of a pull request's head, so an event the intake gives
-- up on would leave its pull request with no head: the intake acknowledges an envelope after 30
-- failed deliveries (delivery.headWriteMaxDeliveries), which a GitHub outage of about five
-- minutes reaches, and acknowledges GitHub's 404 for a pull request, which a read made a moment
-- after GitHub's own webhook can get. The reconcile reads only the pull_request_heads rows that
-- exist, never reads a merged or closed row again, and the read-through runs only for a result's
-- commit. So the intake records the pull request here before it acknowledges such an envelope,
-- and the reconcile's head pass reads each one from GitHub until GitHub answers
-- (delivery.Reconcile.readOwedPullRequests). A row holds no head, no state and no issue, and no
-- reader of a head reads this table, so an owed read is never a head, a basis or a merge.
--
-- The two reasons have a budget each in a pass, and every failure is read before any refusal:
-- any bearer of Envoy's /v1 publish route can name a pull request GitHub never had, and each such
-- envelope owes a refusal, so refusals must take no read from a failure (dispatch://INFRA-3911
-- owns forged envelopes). Inside each reason the row read the fewest times goes first, then the
-- one owed longest, so every row has its first read before any row has its second. The index
-- serves that order, so a table of forged refusals costs a pass an index scan of its budget and
-- not a sort of every row.
--
-- A new table that names no other: the transaction locks only it, which no other session can
-- reach before the commit, and nothing existing is read, locked or rewritten. The release before
-- this one neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md,
-- "Database migrations"). The census answers 0. Rolling back past it is an image rollback alone;
-- the reads still owed then wait for the next event of each pull request, as they did before
-- this release.
create table pull_request_reads_owed (
    repo text not null,
    number integer not null,
    reason text not null check (reason in ('failure', 'refusal')),
    reads integer not null default 0,
    owed_at timestamptz not null default now(),
    primary key (repo, number)
);

create index pull_request_reads_owed_order on pull_request_reads_owed (reason, reads, owed_at, repo, number);
