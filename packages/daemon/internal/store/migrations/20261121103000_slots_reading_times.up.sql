-- 20261121103000_slots_reading_times.up.sql — the times the slots reading dates a slot holder's
-- state by, where the record held none, and the tick notice's slots reading.
--
-- The slots reading (record.SlotsReading, `admission.slots` of GET /legion/v1/state, the line
-- `legion state` prints after its admission count, and the tick controller notice's `slots`)
-- names what each holder of an admission slot is doing and since when (record.HolderStateOf). Three
-- of those times were held nowhere, and this migration adds a place for each, written when the
-- state begins:
--
--   - issue_holds.held_at is when an issue's hold began (record.Hold.Since). The engine's move into
--     held, the one path into that phase, writes it in the transaction that writes the hold
--     (workflow's claimFailed, record.Store.MarkHoldBegan), and a later hold of the same issue
--     replaces it. The row goes with its issue.
--   - slot_waits.since is when a slot holder began to wait on another issue while it keeps its
--     slot (record.Slot.WaitsSince). The admission pass writes it when the holder's waits go from
--     none to some and deletes it when they end (admit's markSlotWaits, record.Store.MarkSlotWait).
--     The row goes with the slot's.
--   - claims.stopped_at is when a suspended or a retired claim stopped. The claim store's write
--     stamps it on the move into either from a state that is neither and clears it on any other
--     state (the claim store's upsertClaim). A failed claim is dated by its failed_at, as before.
--
-- issue_holds and slot_waits are tables of their own rather than columns of issues and slots, so
-- neither of those tables changes shape. Every time is written from the daemon's clock, never by
-- a column default: a replay of a recorded window writes its simulated times there.
--
-- Every row the tables would hold for a state that began before this release is absent, and every
-- claim stopped before it holds no stopped_at: a holder in such a state reads its state with no
-- time (JSON null), and a count of the reading takes the oldest of the times it knows.
--
-- The release before this one names none of the three anywhere, so it writes and reads none of
-- them, and after a rollback each would go stale. Nor does that release decode the field `slots`
-- this release writes on the tick controller notice (outbox kind 'controller_notice', notice kind
-- 'tick'): it decodes an outbox payload strictly (record.DecodeOutboxPayload, DisallowUnknownFields)
-- and knows no such field, so a tick notice left queued fails on every attempt and never finishes,
-- and the tick reaches nobody.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon that shares the database stopped and before
-- the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. The tables and the
-- column are one for every project of the database, so every daemon of it moves together, on the
-- way up and on the way back. docs/kubernetes.md, "Rolling back the release that reads the slots",
-- carries the same block, internal/textcheck's TestTheSlotsReadingWayBackIsOneTextInTheHeaderAndTheGuide
-- holds the two to one text, and internal/store's TestTheSlotsReadingWayBackLeavesWhatTheReleaseBeforeReads
-- runs this one.
--
-- The outbox delete takes the tick notices this release queued and had not sent when the daemon
-- stopped. A sent notice leaves no row, since the outbox deletes a row once it is delivered, so
-- these are the only ones standing. What the delete costs is one tick: the next tick of the daemon
-- that runs queues another. The drops and the delete of this migration's row go together. The
-- delete is what makes the version the store reports, its highest recorded row
-- (store.SchemaVersionTx), that release's own again, and at the roll-forward store.apply finds no
-- row of this version and runs this file again, which creates the tables and adds the column anew;
-- the drops are what let it run again at all. What they cost is the times themselves: after the
-- roll-forward each state that began before it reads with no time, as at the first upgrade.
--
-- begin;
-- delete from outbox where kind = 'controller_notice' and payload->>'kind' = 'tick' and payload ? 'slots';
-- drop table if exists slot_waits;
-- drop table if exists issue_holds;
-- alter table claims drop column if exists stopped_at;
-- delete from schema_version where version = 20261121103000;
-- commit;
create table issue_holds (
  issue text primary key references issues (key) on delete cascade,
  held_at timestamptz not null);
create table slot_waits (
  issue text primary key references slots (issue) on delete cascade,
  since timestamptz not null);
alter table claims add column stopped_at timestamptz;
