-- 20261125143901_issues_last_event_at.up.sql — the time and the seq of each issue's newest event,
-- on the issue's row.
--
-- INFRA-5791: the priorities read's newest event under each priority read, for each member issue,
-- the issue's event of last_seq through events_issue_key_seq_key: 7,651 probes and 30,604 shared
-- buffers a read on a seed of production's counts, most of the read's work. last_event_at is that
-- event's created_at and last_event_at_seq its seq, which the statement that appends the event
-- writes in the same `update issues` that advances last_seq (events/broker.go,
-- QueueAppendWithLogins). Where last_event_at_seq equals last_seq the pair is that event's, and the
-- read takes the time from the row; where it differs, or is null, the read probes the event as
-- before (api's priorityReadQuery, member_event).
--
-- Both columns are null on every row written before this migration, and
-- 20261125144002_issues_last_event_at_backfill fills them. The release before this one names its
-- own columns of issues in its writes, so it serves on this schema; an event it appends advances
-- last_seq and leaves the pair as it was, so the pair then names an older seq than last_seq, which
-- the read takes for unknown and probes. A rollback finds the two columns unread.
--
-- Both columns are nullable with no default and no check, so the ALTER writes only the catalog:
-- it reads no row and rewrites none, and holds issues' ACCESS EXCLUSIVE lock for the catalog write
-- alone; the runner's lock_timeout (pgmigrate.LockTimeout, 5 s) bounds the wait for the lock. The
-- fill is a migration of its own so this lock does not stay held while it runs. The identifier is
-- the UTC moment this was written, as `migrationid` requires of every migration past the numbered
-- series.
alter table issues
  add column last_event_at timestamptz,
  add column last_event_at_seq bigint;
