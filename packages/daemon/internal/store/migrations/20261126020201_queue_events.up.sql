-- 20261126020201_queue_events.up.sql — the merge-queue timeline events of each project's pull
-- requests, kept for the queue signal of admission by signals, the queue's boot read status on
-- signal_inputs, and the outbox kind that reads a pull request's timeline after a queue move.
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
--     bounds the 6-hour exit window read (record.Store.QueueEventsAt).
--   - signal_inputs gains read_error (text not null default ''): empty while the input's boot read
--     succeeded (or the input needs none), and non-empty with the failure's reason while the boot
--     read of the input failed, so a failed boot read of the merge queue reads absent with its
--     reason rather than as an empty queue.
--   - outbox_kind_check widens to include queue_timeline_read, the outbox row that reads a pull
--     request's timeline from GitHub outside the fact lock after an enqueued, dequeued or merged
--     notification and applies intake.QueueTimeline.
--
-- Every time is written from GitHub's timeline or the daemon's clock, never by a column default.
--
-- The migration adds one table, adds one defaulted column to signal_inputs and widens
-- outbox_kind_check, changing nothing an earlier release reads. An earlier release neither reads
-- queue_events nor runs queue_timeline_read rows.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon that shares the database stopped and before
-- the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. Deleting
-- queue_timeline_read rows removes the rows an earlier release has no lane for; dropping
-- queue_events and read_error, deleting the queue row of signal_inputs and deleting this version
-- row lets the roll-forward apply this migration anew and record a fresh kept_since at its first
-- boot.
--
-- begin;
-- delete from outbox where kind = 'queue_timeline_read';
-- drop table if exists queue_events;
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

alter table signal_inputs add column read_error text not null default '';

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'queue_timeline_read'));
