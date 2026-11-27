-- 20261118212431_legion_events.up.sql — where Dispatch's intake of each Legion daemon's state
-- events stands.
--
-- dispatch://INFRA-3931: a Legion daemon publishes each change of its state as a state event, one
-- message per committed transaction on notifications.legion.<token>.state, and Dispatch's intake
-- (internal/dispatch/legion/intake.go, on the held durable `legion-state-events`) writes the
-- project's observations from those messages. It reads the daemon's whole state only where the
-- events cannot carry it, for the reasons legion's ReadReason constants name. This migration gives
-- legion_reads (20261103170923) what tells those reads from the batches of events, creates the
-- tables that hold how far each project's observations have followed its daemon and the parts of
-- a transaction still to be completed, and gives server_processes what tells a build with this
-- intake from a build before it.
--
-- legion_reads gains three columns:
--
-- - kind is what the row records: `read`, a whole read of the daemon's state, or `events`, one
--   transaction of state events the intake applied, whose read_at is the message's `at` (the
--   daemon's commit time) and whose bytes are its messages'.
-- - reason is why a whole read was made (legion's ReadReason constants), and null for a batch of
--   events and for the sampler's minute.
-- - recorded_at is when Dispatch wrote the row. Beside read_at it is how long an event took to
--   reach Dispatch, since a batch's read_at is the daemon's own time.
--
-- legion_reads_project_recorded_at orders a project's rows as Dispatch wrote them, by recorded_at
-- and then id: the Legion board's header reads a project's newest row by it, and
-- legion.PruneReads keeps each project's newest row by it. A batch's read_at is the daemon's clock
-- and a whole read's is Dispatch's, so legion_reads_project_read_at (20261103170923) does not order
-- the two kinds.
--
-- legion_event_cursors is one row per project, which the intake inserts, following no daemon, as
-- a whole read of a project with a setting begins (legion's writeReadStart) or as a step locks a
-- project with a setting and no row (lockCursor), so a row can exist before any successful read
-- and any applied event. Each batch and each read reads it `for update` and writes it in its
-- transaction:
--
-- - project is the project's own key and the row's identity, and goes with the project.
-- - state_url is the daemon the row follows: the setting's state_url when the row was written. A
--   row naming another daemon than the setting is no starting point, so the intake reads the
--   setting's daemon whole. A write of the setting (legion.PutSettings) and a delete of it
--   (legion.DeleteSettings) either leave the row as it is or reset it, by the rule that legion's
--   resetCursor and cursor.reset hold whole (read-rule-cases rows 6.1 to 6.5). A reset sets
--   state_url to '', which no setting names, and every column below but last_read_started_at,
--   previous_state_id, previous_state_url and released_holders to its empty value, and it keeps
--   the row.
-- - run is the daemon run (the UUID it minted at boot) of the newest message the intake applied, or
--   the daemon.stateRun of the daemon that served a successful whole read made after it, where that
--   daemon served one; null before either. A boot at or below last_seq under another run is a
--   daemon whose sequence went back, which the intake reads whole; one under this run is one the
--   cursor holds.
-- - last_seq is the newest state sequence the project's observations reflect: the stateSeq a
--   successful whole read served, or the seq of the newest transaction applied. Null while the row
--   holds none, which is only while a read is owed or the row follows no daemon (state_url '').
-- - read_reasons is every reason a whole read is owed for (legion's ReadReason constants), in the
--   order they called for it, and empty while none is owed. legion's cursor.readFor says when a
--   call adds its reason, and cursor.afterRead what a read leaves. The read is recorded under the
--   first reason and logged with every reason it joined; a starting read joins start alone and
--   clears the rest (cursor.readFor).
-- - read_owed is a whole read owed, which Postgres generates from read_reasons, so no write names
--   it.
-- - last_read_started_at is when the project's newest whole read began, whatever its reason and
--   whatever it came to. Every whole read writes it before it calls the daemon, the later of the
--   row's value and the read's start, in the statement that empties skipped_state_ids and
--   skipped_seqs: a project with a setting and no row is given its row there (state_url ''), and a
--   project with no setting is given none. The read writes its start first because a read that a
--   stop of the intake cuts short records nothing else, and the next holder of the durable must
--   find that start (read-rule-cases row 7.6). Every read's record, a good starting read's
--   included, sets it again, and so does a read whose record the intake drops because the setting
--   names another URL by then (legion's recordWhole), the later of the two kept. The next whole
--   read of the project, for any reason and the starting read included, waits a minute after it
--   (legion.ReadRetryAfter; R3: whole reads of one project start at least 60 seconds apart), so a
--   read the daemon is slow to answer, or never answers, adds no time to the minute. A reset of the
--   row keeps it (state_url above), so the starting reads of two writes within a minute start a
--   minute apart, with a delete between them or none. Null only while no whole read has begun
--   since the row was created.
-- - read_holder is the stream sequence of the state message that holds the owed read: the message
--   the intake sent back to the stream unacknowledged until the read is due, whose redelivery on
--   the project's runner makes the read, whatever store the message is of. Null where no message
--   holds it. Which message holds it is the rule of legion's cursor.readFor, and what a read leaves
--   of it the rule of cursor.afterRead.
-- - read_holds is how many times the message read_holder names has gone back to the stream
--   holding the read: 1 when the intake records it as the holder, one more at each hand-back after
--   it, and 0 where no message holds it. A lease take-over, which starts the message's own
--   delivery count again, keeps this one. The bound it counts toward, and how long a holder can
--   hold the durable's ack floor, is legion's stateMaxHolds.
-- - state_id is the id of the daemon's store (the state route's daemon.stateId, which every state
--   message carries as stateId): a successful whole read sets it, a reset of the row (state_url
--   above) clears it, and nothing else writes it. The intake applies no message of another
--   id, and none at all while it is null, which is only while a read is owed or the row follows no
--   daemon.
-- - schema_version is the store's schema version as the row last saw it: the daemon.schemaVersion
--   of a successful whole read, or the schemaVersion of the newest boot event applied. A boot of
--   another version is read whole. Null for none.
-- - previous_state_id is the store of the daemon the setting named before a write that named
--   another daemon, or before a delete of the setting, which a reset of the row sets and clears
--   (legion's cursor.reset; read-rule-cases rows 6.3, 6.4 and 6.5). The intake applies no message
--   of this store while state_id is null or another store: R3 applies no message of the daemon the
--   setting named before. What such a message calls for, and when it holds a read, is the rule of
--   legion's cursor.drops and cursor.finishesUnapplied. A good whole read whose state_id is this
--   store clears it, and every other whole read keeps it. Null for none.
-- - previous_state_url is the URL the row followed when previous_state_id was set, and goes with
--   it: a write of this URL names that daemon again, so its reset clears both columns (legion's
--   cursor.reset). A good whole read whose state_id is previous_state_id clears both too: the
--   daemon is the one the setting names, behind another URL. The intake then judges the daemon's
--   messages as the messages of the setting's daemon. Every other whole read keeps it. Null for
--   none.
-- - foreign_state_ids is each store that is no daemon of the project, the newest last, at most
--   legion's maxForeignStores, past which the oldest goes. Which steps add a store is the rule of
--   legion's cursor.marking. A good read whose state_id is in it takes that store out, and a reset
--   of the row empties it (read-rule-cases row 6.4b). The intake treats a message of a store in it
--   as a message of previous_state_id (above). Empty for none.
-- - released_holders is the stream sequence of each state message whose hold of the owed read a
--   reset of the row ended (read-rule-cases rows 6.3b and 6.5), the newest last. At such a
--   message's next delivery while the project has a setting, the intake acknowledges it unapplied,
--   it holds no read, its store goes into foreign_state_ids as above, and the intake takes its
--   sequence out of the array, and no other. So a write that names another URL ends the hold of the
--   message before it within the holder's remaining hand-back delay (60 s at most) plus one read's
--   60 s and a second, however many such writes come before that message comes back, and a message
--   of the newly named daemon may hold the read from a count of 1. When an entry is added and when
--   it goes is the rule of legion's cursor.reset. Empty for none.
-- - skipped_state_ids and skipped_seqs are, pair by pair (the check holds them to one length), a
--   store and the highest seq of a state message of that store that the intake acknowledged
--   unapplied because it arrived while legion's stateMaxWaiting messages of the project waited,
--   the store skipped last at the end, at most legion's maxSkippedStores stores, past which the
--   store skipped longest ago goes. Every whole read empties both as it begins, since the daemon
--   commits each event before it publishes it, so the read holds every message published before
--   it, and a reset of the row empties them. What a read owes for what they hold as it lands, and
--   whether it empties them, is the rule of legion's cursor.afterRead. Empty for none.
--
-- legion_event_parts holds the messages of a transaction the daemon split over several messages
-- (each but the last carries `more: true`) until its last message arrives: the intake then applies
-- every event of the transaction in one batch and deletes the parts, so no observation holds a
-- value of a transaction it has not read whole. A part is held only where it follows the cursor or
-- the part before it, under the same run and store. Each reset of the cursor's row deletes the
-- project's parts (legion_event_cursors.state_url above), and legion's recordWhole says which whole
-- reads delete them:
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
-- for a row of a build before this one, whose sampler reads each daemon whole every minute. What
-- the intake does while a row at 0 is in the fleet (processes.EarlierLegionIntake) is the rule of
-- legion's startBlocked.
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
  last_read_started_at timestamptz,
  read_holder bigint,
  read_holds integer not null default 0,
  state_id text,
  schema_version bigint,
  previous_state_id text,
  previous_state_url text,
  foreign_state_ids text[] not null default '{}',
  released_holders bigint[] not null default '{}',
  skipped_state_ids text[] not null default '{}',
  skipped_seqs bigint[] not null default '{}',
  check (cardinality(skipped_state_ids) = cardinality(skipped_seqs))
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
