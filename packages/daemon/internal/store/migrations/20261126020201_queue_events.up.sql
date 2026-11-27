-- 20261126020201_queue_events.up.sql — the merge-queue timeline events of each project's pull
-- requests and how many entries and exits of each the daemon has kept, for the queue signal of
-- admission by signals, and the queue's boot read status on signal_inputs.
--
-- The queue signal (signals.QueueAt) reads the waits of a project's pull requests in GitHub's
-- merge queue from GitHub's own timeline events (added_to_merge_queue, removed_from_merge_queue
-- and merged), folded by signals.WaitsOf. Admission keeps each event once per project by GitHub's
-- event id (admit's keepSignalFacts, record.Store.KeepQueueEvents), with the canonical repository
-- GitHub's event url names, the pull request number, GitHub's created_at, whether the event enters
-- the queue, and its position in GitHub's timeline so events in the same second keep GitHub's
-- order:
--
--   - queue_events holds one row per GitHub timeline event per project. The primary key is
--     (project, event_id), so an event brought by both a webhook timeline read and a boot read,
--     or by a duplicate delivery, is kept once. The index (project, repo, pull_request, at,
--     position, event_id) serves the per-PR history read in GitHub's order, and (project, at)
--     bounds the 6-hour exit window read (record.Store.QueueEventsAt). A pull request whose newest
--     event is an exit 6 hours or more old has its rows deleted, since no read counts them again.
--   - queue_progress holds, for each pull request of a project under the repository its events
--     name, how many entries (entered) and exits (exited) of it the daemon has kept, each the
--     greatest count of kept events of that direction any keep saw. The delete above leaves it, so
--     the count a notified move is checked against (record.QueueProgress.Needs) still holds the
--     moves whose events were deleted, and a timeline read that shows only those is read again.
--     The primary key (project, pull_request, repo) serves the read of one pull request under each
--     name a rename pairs (record.Store.QueueProgressOf). It holds one row for each pull request
--     that ever moved in the queue, and no read lists them.
--   - signal_inputs gains read_error (text not null default ''): empty while the input's boot read
--     succeeded (or the input needs none), and non-empty with the failure's reason while the boot
--     read of the input failed, so a failed boot read of the merge queue reads absent with its
--     reason rather than as an empty queue.
--
-- The outbox kind that reads a pull request's timeline after a queue move, queue_timeline_read,
-- is admitted by 20261126190955_outbox_kinds_with_the_queue_timeline_read, whose version stands
-- above every list of the kind check. This migration writes no list, so its way back and its roll
-- forward leave the kind check as it stands.
--
-- Every time is written from GitHub's timeline or the daemon's clock, never by a column default.
--
-- The migration adds two tables and adds one defaulted column to signal_inputs, changing nothing
-- an earlier release reads. An earlier release neither reads queue_events or queue_progress nor
-- runs queue_timeline_read rows.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon that shares the database stopped and before
-- the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. Deleting
-- queue_timeline_read rows removes the rows an earlier release has no lane for; dropping
-- queue_events, queue_progress and read_error, deleting the queue row of signal_inputs and
-- deleting this version row lets the roll-forward apply this migration anew and record a fresh
-- kept_since at its first boot.
--
-- begin;
-- delete from outbox where kind = 'queue_timeline_read';
-- drop table if exists queue_events;
-- drop table if exists queue_progress;
-- alter table signal_inputs drop column if exists read_error;
-- delete from signal_inputs where input = 'queue';
-- delete from schema_version where version = 20261126020201;
-- commit;
create table queue_events (
    project      text        not null check (project <> ''),
    event_id     bigint      not null check (event_id >= 1),
    repo         text        not null check (repo <> ''),
    pull_request integer     not null check (pull_request >= 1),
    at           timestamptz not null,
    enters       boolean     not null,
    position     integer     not null check (position >= 0),
    primary key (project, event_id)
);
create index queue_events_by_pull on queue_events (project, repo, pull_request, at, position, event_id);
create index queue_events_by_time on queue_events (project, at);

create table queue_progress (
    project      text    not null check (project <> ''),
    repo         text    not null check (repo <> ''),
    pull_request integer not null check (pull_request >= 1),
    entered      integer not null check (entered >= 0),
    exited       integer not null check (exited >= 0),
    primary key (project, pull_request, repo)
);

alter table signal_inputs add column read_error text not null default '';
