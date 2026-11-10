-- 20261110030133_ci_reconcile_passes.up.sql
--
-- INFRA-1179: one row per CI reconcile pass, so the counts a pass reports outlive the container
-- log that carries its one INFO line. The log rotates fast -- the production pod's reached back
-- only 45 minutes -- and a person asking why a head has not settled needs the passes of the last
-- day, not of the last three quarters of an hour. The pass writes the row when it starts and
-- fills the rest in when it ends (internal/dispatch/cireconcile, Reconciler.beginPass and
-- Reconciler.endPass), and the pass that ends deletes every row older than 48 hours, so the
-- table holds about 576 rows of a deployment running one pass every five minutes.
--
--   id              the row's own identity, and the order two rows of one instant are read in.
--   instance        the server_processes row of the process that ran the pass
--                   (internal/dispatch/processes, Register). It carries no foreign key to that
--                   table on purpose: a process deletes its registry row when its HTTP drain
--                   ends, and these rows must outlive the process by a day, which a foreign key
--                   -- on delete cascade or restrict -- would make impossible either way.
--                   GET /api/v1/ci-reconcile/passes joins the two by this column to ask whether
--                   the process is still live, and reads a pass of a process that has gone as
--                   one that ended without completing.
--   started_at      when the pass started, written by the insert.
--   ended_at        when it ended, null while it is running and for a pass whose process died
--                   before it could write its end. The route tells those two apart by the
--                   process's own liveness, by the `ci-reconcile` lease and by whether the row
--                   is the newest one, never by the null alone.
--   outcome         how the pass ended: 'complete', 'deadline' (its own five-minute bound cut it
--                   short) or 'rate_limit' (a shared GitHub limit ended it where it stood). Null
--                   exactly while ended_at is null, which the check below holds the pair to.
--   the counts      the eight numbers the pass's one log line carries: the GraphQL queries it
--                   asked and the points GitHub charged them, the REST requests beside them and
--                   how many of those GitHub answered 304, the heads it had to read over REST,
--                   the CI records it changed, the heads it read whole and the heads it knows
--                   there are. Null while the pass runs: a pass that has not ended has counted
--                   nothing yet, and a zero would read as a pass that found nothing to do.
--   the cursor      where the pass left the order for the pass after it: the mapped repository,
--                   the page of its open pull requests, and the first pull request of that page
--                   the pass did not finish. A pass that reached the end of the order leaves the
--                   zero cursor, which is the start of the order. GitHub's own opaque page token
--                   is not stored: it is valid only to the process that was handed it, and it
--                   says nothing to a person reading the row.
--
-- A new table nothing before this release names: the release before this one neither reads nor
-- writes it, so the two roll side by side (cmd/dispatch/README.md, "Database migrations"), and
-- the census answers 0 because the table holds no row at this schema. Rolling back past it is an
-- image rollback alone. The identifier is the UTC moment this file was created, as every
-- migration past the numbered series is named.
create table ci_reconcile_passes (
    id bigint generated always as identity primary key,
    instance uuid not null,
    started_at timestamptz not null default now(),
    ended_at timestamptz,
    outcome text check (outcome in ('complete', 'deadline', 'rate_limit')),
    graphql_queries integer,
    graphql_points integer,
    rest_requests integer,
    rest_not_modified integer,
    heads_over_rest integer,
    records integer,
    heads integer,
    heads_known integer,
    cursor_repository text,
    cursor_page integer,
    cursor_number bigint,
    constraint ci_reconcile_passes_ended_with_an_outcome check ((ended_at is null) = (outcome is null))
);

-- The one read the route makes: the passes of the last 24 hours, newest first. The prune's
-- delete of everything older than 48 hours reads the same order from the other end.
create index ci_reconcile_passes_started_at on ci_reconcile_passes (started_at desc, id desc);
