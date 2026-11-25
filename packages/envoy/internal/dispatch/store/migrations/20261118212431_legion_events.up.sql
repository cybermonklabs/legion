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
--   setting's daemon whole. A write of the setting (legion.PutSettings) and a delete of it
--   (legion.DeleteSettings) either leave the row as it is or reset it, by the rule that legion's
--   resetCursor and cursor.reset hold whole (read-rule-cases rows 6.1 to 6.5). A reset sets
--   state_url to '', which no setting names, and every column below but last_read_started_at,
--   previous_state_id, previous_state_url and released_holders to its empty value, and it keeps
--   the row: that is what makes a PUT of the same URL a fresh whole read, made once a minute has
--   passed since the project's newest whole read began.
-- - run is the daemon run (the UUID it minted at boot) of the newest message the intake applied, or
--   the daemon.stateRun of the daemon that served a successful whole read made after it, where that
--   daemon served one; null before either. A boot at or below last_seq under another run is a
--   daemon whose sequence went back, which the intake reads whole; one under this run is one the
--   cursor holds.
-- - last_seq is the newest state sequence the project's observations reflect: the stateSeq a
--   successful whole read served, or the seq of the newest transaction applied. Null while the row
--   holds none, which is only while a read is owed or the row follows no daemon (state_url '').
-- - read_reasons is every reason a whole read is owed for (legion's ReadReason constants), in the
--   order they called for it, and empty while none is owed: a whole read that failed owes it for
--   `retry`, a message that calls for a read inside a minute of the last one adds its reason, and a
--   read clears the reasons the row held when the read was decided. A reason a message added while
--   the read was in flight stays on a failed read, since that message's events may be past the
--   read's snapshot. A successful read clears it too, but for two cases. Where skipped_state_ids
--   and skipped_seqs hold a sequence of the read's store above the read's stateSeq, the read keeps
--   it and owes `gap` with it, whatever the row owed when the read was decided. Where they hold a
--   store other than the read's, which neither previous_state_id nor foreign_state_ids names as
--   the read leaves them, the read keeps it and owes `store` with it. The read is recorded under
--   the first reason and logged with them all.
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
--   moved meanwhile, the later of the two kept. The next whole read of the project, for any reason
--   and the starting read included, waits a minute after it (legion.ReadRetryAfter; R3: whole
--   reads of one project start at least 60 seconds apart), so a read the daemon is slow to answer,
--   or never answers, adds no time to the minute. A reset of the row keeps it (state_url above),
--   so the starting reads of two writes within a minute start a minute apart, with a delete
--   between them or none. Null only while no whole read has begun since the row was created.
-- - read_holder is the stream sequence of the state message that holds the owed read: the
--   message the intake sent back to the stream unacknowledged until the read is due, whose
--   redelivery makes the read, whatever store the message is of. Null where no message holds it.
--   The first message that arrives past legion's stateMaxWaiting while a whole read is in flight
--   takes it over, for the read after that one. A read that succeeded clears it, unless that read
--   leaves a reason owed: the message then holds that reason. A read that failed keeps it.
-- - read_holds is how many times the message read_holder names has gone back to the stream
--   holding the read: 1 when the intake records it as the holder, one more at each hand-back after
--   it, and 0 where no message holds it. The intake acknowledges the holder once it has gone back
--   10 times (legion's stateMaxHolds) and clears both columns. A lease take-over creates the
--   intake's durable again, which starts the message's own delivery count again, and keeps this
--   count, and a write of the setting either leaves the row as it is, this count included, or
--   ends the hold (state_url above, released_holders), so the holder goes back 10 times at the
--   most in all and holds the durable's ack floor for about 11 minutes at the most.
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
--   (legion's cursor.reset; read-rule-cases rows 6.3, 6.4 and 6.5). The intake finishes each
--   message of this store unapplied while state_id is null or another store (legion's
--   outcomeForgotten): R3 applies no message of the daemon the setting named before. Such a
--   message waits on the project's runner behind a whole read in flight, for one read's 60 s at
--   the most, as every message does, and is judged against the row that read leaves. It holds
--   no read. Whatever state_id holds, it calls for the owed read where that read is due and no
--   message holds it, and the intake makes the read after it acknowledges the message, at most
--   once a minute, since the setting's own daemon can be redeployed into this store; it calls
--   for no read of its own. A good whole read whose state_id is this store clears it, and every
--   other whole read keeps it. Null for none.
-- - previous_state_url is the URL the row followed when previous_state_id was set, and goes with
--   it: a write of this URL names that daemon again, so its reset clears both columns (legion's
--   cursor.reset). A good whole read whose state_id is previous_state_id clears both too: the
--   daemon is the one the setting names, behind another URL. The intake then judges the daemon's
--   messages as the messages of the setting's daemon. Every other whole read keeps it. Null for
--   none.
-- - foreign_state_ids is each store that is no daemon of the project, the newest last, 8 at the
--   most (legion's maxForeignStores): past 8 the oldest goes. A good whole read made for a message
--   of another store (`store`) that finds state_id unchanged adds that message's store, and so
--   does each message whose hold of the owed read released_holders records, at its next delivery,
--   unless its store is previous_state_id or state_id. A good read whose state_id is in it takes
--   that store out, and a reset of the row empties it (read-rule-cases row 6.4b). The intake
--   treats a message of a store in it as a message of previous_state_id: it finishes it unapplied
--   while state_id is null or another store, and the message holds no read and calls for no read
--   of its own, only the owed read as above. So a daemon that the setting named, whose reads all
--   failed and which still publishes after the setting named a daemon that a read verified, makes
--   one `store` read of its own at the most, and none after it, unless it boots: a boot of a store
--   in it, or of previous_state_id, while the row follows the setting's URL in another store a
--   read verified, calls for a `store` read, so a daemon that a redeploy started behind that URL
--   in such a store is found again. A read that finds the row's store unchanged leaves the mark,
--   and never marks previous_state_id. Empty for none.
-- - released_holders is the stream sequence of each state message whose hold of the owed read a
--   reset of the row ended (legion's cursor.reset; read-rule-cases rows 6.3b and 6.5): the newest
--   last, 8 at the most (legion's maxReleasedHolders). At such a message's next delivery while the
--   project has a setting, the intake acknowledges it unapplied, it holds no read, its store goes
--   into foreign_state_ids as above, and the intake takes its sequence out of the array, and no
--   other. So a write that names another URL ends the hold of the message before it within one
--   read's 60 s and a second, however many such writes come before that message comes back, and a
--   message of the newly named daemon may hold the read from a count of 1. While the project has
--   no setting after a delete, the intake drops the message before it reads the row, so its entry
--   can stay until 8 newer entries push it out. Past 8 the oldest entry goes: its message is
--   judged as any message at its next delivery, which applies nothing of a store other than
--   state_id, and it can hold a read again from a count of 1. Empty for none.
-- - skipped_state_ids and skipped_seqs are, pair by pair (the check holds them to one length), a
--   store and the highest seq of a state message of that store that the intake acknowledged
--   unapplied because it arrived while legion's stateMaxWaiting messages of the project waited,
--   the store skipped last at the end, 8 stores at the most (legion's maxSkippedStores): past 8 the
--   store skipped longest ago goes. Every whole read empties both as it begins, since the daemon
--   commits each event before it publishes it, so the read holds every message published before
--   it; what they hold as the read lands was skipped while it was in flight, and such a message's
--   events may be past its snapshot. So a successful read that finds a seq of its own store above
--   its stateSeq still owes `gap` (read_reasons). One that finds a store other than its own, which
--   neither previous_state_id nor foreign_state_ids names as the read leaves them, still owes
--   `store`: that message came after the read began, so the read does not answer it. Every
--   successful read empties both. A read that failed keeps them, and a reset of the row empties
--   them. Empty for none.
--
-- legion_event_parts holds the messages of a transaction the daemon split over several messages
-- (each but the last carries `more: true`) until its last message arrives: the intake then applies
-- every event of the transaction in one batch and deletes the parts, so no observation holds a
-- value of a transaction it has not read whole. A part is held only where it follows the cursor or
-- the part before it, under the same run and store. A successful whole read deletes the project's
-- parts, and so does each reset of the cursor's row (legion_event_cursors.state_url above); a whole
-- read that failed keeps them:
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
