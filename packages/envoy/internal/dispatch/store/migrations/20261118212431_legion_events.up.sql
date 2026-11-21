-- 20261118212431_legion_events.up.sql — where Dispatch's intake of each Legion daemon's state
-- events stands.
--
-- dispatch://INFRA-3931: a Legion daemon publishes each change of its state as a state event, one
-- message per committed transaction on notifications.legion.<token>.state, and Dispatch's intake
-- (internal/dispatch/legion/intake.go, on the held durable `legion-state-events`) writes the
-- project's observations from those messages. It reads the daemon's whole state only where the
-- events cannot carry it: a project with no starting point, a boot that does not follow or that
-- moved the store's schema, a gap the stream no longer holds, a message of another store, a message
-- the database refused, and a read owed since one failed. This migration gives legion_reads
-- (20261103170923) what tells those reads from the batches of events, creates the tables that hold
-- how far each project's observations have followed its daemon and the parts of a transaction
-- still to be completed, and gives server_processes what tells a build with this intake from a
-- build before it.
--
-- legion_reads gains three columns:
--
-- - kind is what the row records: `read`, a whole read of the daemon's state, or `events`, one
--   transaction of state events the intake applied, whose read_at is the message's `at` (the
--   daemon's commit time) and whose bytes are its messages'.
-- - reason is why a whole read was made (legion's ReadReason constants: `start`, `boot`,
--   `schema`, `gap`, `retry`, `store`, `refused`), and null for a batch of events and for the
--   sampler's minute.
-- - recorded_at is when Dispatch wrote the row. Beside read_at it is how long an event took to
--   reach Dispatch, since a batch's read_at is the daemon's own time.
--
-- legion_reads_project_recorded_at orders a project's rows as Dispatch wrote them, by recorded_at
-- and then id: the Legion board's header reads a project's newest row by it, and
-- legion.PruneReads keeps each project's newest row by it. A batch's read_at is the daemon's clock
-- and a whole read's is Dispatch's, so legion_reads_project_read_at (20261103170923) does not order
-- the two kinds.
--
-- legion_event_cursors is one row per project whose events the intake has applied or whose state
-- it has read whole, read `for update` and written in the transaction of each batch and each read:
--
-- - project is the project's own key and the row's identity, and goes with the project.
-- - state_url is the daemon the row follows: the setting's state_url when the row was written. A
--   row naming another daemon than the setting is no starting point, so the intake reads the
--   setting's daemon whole. Every write of the setting deletes the row (legion.PutSettings and
--   legion.DeleteSettings), which is what makes a PUT of the same URL a fresh whole read.
-- - run is the daemon run (the UUID it minted at boot) of the newest message the intake applied, or
--   the daemon.stateRun of the daemon that served a successful whole read made after it, where that
--   daemon served one; null before either. A boot at or below last_seq under another run is a
--   daemon whose sequence went back, which the intake reads whole; one under this run is one the
--   cursor holds.
-- - last_seq is the newest state sequence the project's observations reflect: the stateSeq a
--   successful whole read served, or the seq of the newest transaction applied. Null while the row
--   holds none, which is only while a read is owed.
-- - read_reasons is every reason a whole read is owed for (legion's ReadReason constants), in the
--   order they called for it, and empty while none is owed: a whole read that failed owes it for
--   `retry`, a message that calls for a read inside a minute of the last one adds its reason, and a
--   read that succeeded clears them. The read is recorded under the first and logged with them
--   all.
-- - read_owed is a whole read owed, which Postgres generates from read_reasons, so no write names
--   it.
-- - read_tried_at is when the whole read the next one waits a minute after was tried, whatever
--   the next one's reason: each read but a good starting read sets it, and a good starting read
--   clears it, since no message called for it.
-- - read_holder is the stream sequence of the state message that holds the owed read: the
--   message the intake sent back to the stream unacknowledged until the read is due, whose
--   redelivery makes the read. Null where no message holds it. A read that succeeded clears it, and
--   one that failed keeps it.
-- - state_id is the id of the daemon's store (the state route's daemon.stateId, which every state
--   message carries as stateId): a successful whole read sets it, and nothing else does. The
--   intake applies no message of another id, and none at all while it is null, which is only while
--   a read is owed.
-- - schema_version is the store's schema version as the row last saw it: the daemon.schemaVersion
--   of a successful whole read, or the schemaVersion of the newest boot event applied. A boot of
--   another version is read whole. Null for none.
--
-- legion_event_parts holds the messages of a transaction the daemon split over several messages
-- (each but the last carries `more: true`) until its last message arrives: the intake then applies
-- every event of the transaction in one batch and deletes the parts, so no observation holds a
-- value of a transaction it has not read whole. A part is held only where it follows the cursor or
-- the part before it, under the same run and store. A successful whole read deletes the project's
-- parts, and so does the cursor's delete (the foreign key), which every write of the setting
-- makes; a whole read that failed keeps them:
--
-- - project and first_seq are the part's identity: the project, and the first sequence the part
--   carries. last_seq is its last.
-- - run and state_id are the message's run and store, which every part of one transaction shares.
-- - at is the transaction's commit time, which every part shares, and bytes the message's size.
-- - events is the message's events as the daemon sent them, the JSON text of their array: text, not
--   jsonb, so a held part decodes as its message did, the values the observations' own encoding
--   rewrites (legion's encodeValue) included.
--
-- server_processes (migration 0135) gains legion_state_intake: 1 for a process whose build takes
-- the Legion observations from the state events (processes.LegionStateIntake), and 0, the default,
-- for a row of a build before this one, whose sampler reads each daemon whole every minute. The
-- intake takes no starting point while a row at 0 has heartbeated within the owner lock's window
-- for an earlier build (processes.EarlierLegionIntake), so no sampler row lands after a starting
-- point.
--
-- The defaults are what let a pod of the release before this one go on during the rolling release:
-- its insert into legion_reads names none of the three columns and reads as a whole read
-- (`kind = 'read'`) with no reason, recorded at its own transaction's time; its insert into
-- server_processes names its columns and not legion_state_intake, so its row reads 0; and it never
-- names legion_event_cursors or legion_event_parts.
--
-- Each column is added with a default every existing row meets: a constant one, and now(), which
-- is stable, so every row stored already reads this migration's moment as its recorded_at and none
-- is rewritten. The check holds the new column alone, which every existing row meets at its
-- default, so it refuses nothing; it is validated by a scan of legion_reads (a week of reads at the
-- most, legion.ReadRetention) under the ACCESS EXCLUSIVE lock the three columns take. The index is
-- built over the same rows under the same lock. server_processes takes ACCESS EXCLUSIVE for the
-- catalog write of its column, until the migration commits: a heartbeat waits that long, and the
-- scan of legion_reads is the longest step, a week of reads. The new tables no earlier release
-- names, and their foreign keys hold projects and legion_event_cursors in SHARE ROW EXCLUSIVE until
-- the migration commits. Nothing existing is refused or rewritten, and the census answers 0. The
-- identifier is the UTC moment this was written, as `migrationid` requires of every migration past
-- the numbered series.
alter table legion_reads
  add column kind text not null default 'read' check (kind in ('read', 'events')),
  add column reason text,
  add column recorded_at timestamptz not null default now();

create index legion_reads_project_recorded_at on legion_reads (project, recorded_at desc, id desc);

create table legion_event_cursors (
  project text primary key references projects(key) on delete cascade,
  state_url text not null,
  run text,
  last_seq bigint,
  read_reasons text[] not null default '{}',
  read_owed boolean generated always as (cardinality(read_reasons) > 0) stored,
  read_tried_at timestamptz,
  read_holder bigint,
  state_id text,
  schema_version bigint
);

create table legion_event_parts (
  project text not null references legion_event_cursors(project) on delete cascade,
  first_seq bigint not null,
  last_seq bigint not null,
  run text not null,
  state_id text not null,
  at timestamptz not null,
  bytes integer not null,
  events text not null,
  primary key (project, first_seq)
);

alter table server_processes
  add column legion_state_intake integer not null default 0;
