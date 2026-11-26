-- packages/envoy/internal/cipher/store/migrations/0044_nats_hold.up.sql
-- The NATS writer hold and every record its workflow trusts, which the auth service
-- (internal/cipher/natscallout) and the operator's commands read. They live here, not in NATS,
-- because any NATS user with a JetStream API grant can have the server store an answer over a key
-- of its choosing; no NATS user can write these tables. Every row carries the operator's Ed25519
-- signature over its lines (internal/bus/signedrecord), so a database writer other than the
-- operator, a database admin or the other service that shares the instance, writes a row that
-- every reader reads as unknown, never as released.
--
-- nats_hold is the one hold row (id 1): its gen, which only goes up, its phase, the hold's id,
-- the reason, who wrote it and when, and the time it was signed with its signature. The migration
-- inserts no row: the first row is the operator's signed released, written by
-- POST /v1/nats-hold/restore, and until a row that verifies exists the auth service, when it reads
-- the hold at all (BROKER_NATS_HOLD_READ), refuses every client but the operator.
create table nats_hold (
  id smallint primary key check (id = 1),
  gen bigint not null check (gen >= 0),
  phase text not null check (phase in ('released', 'prepare', 'held')),
  hold_id text not null,
  reason text not null,
  updated_at timestamptz not null,
  updated_by text not null,
  signed_at timestamptz not null,
  sig text not null
);

-- nats_hold_log is one row for each write of nats_hold, appended by the write's own statement, so
-- the history is kept and a restored row's gen can be set above every earlier one. Each carries the
-- row the write stored as the operator signed it, its signing time and signature, so a restore
-- judges by the latest entry only once it verifies: a database writer can add or change a log row,
-- but not one that verifies.
create table nats_hold_log (
  gen bigint primary key,
  phase text not null,
  hold_id text not null,
  reason text not null,
  written_at timestamptz not null,
  written_by text not null,
  signed_at timestamptz not null,
  sig text not null
);

-- nats_hold_records is each hold's records, one row per kind and key: the cohort, the conversion's
-- saved set, the journal (kind done, key hold) and the selection-recovery inputs. Each row's gen
-- starts at 1 and goes up by one with each write, which names the gen it expects.
create table nats_hold_records (
  hold_id text not null,
  kind text not null check (kind in ('cohort', 'convert', 'done', 'selection')),
  key text not null,
  value jsonb not null,
  gen bigint not null check (gen >= 1),
  written_at timestamptz not null,
  sig text not null,
  primary key (hold_id, kind, key)
);

-- nats_hold_record_deletes is each record the routes deleted, the cohort that hold abort from
-- prepare deletes, one row per kind and key: the gen it was deleted at, when, and the operator's
-- signature over the delete (signedrecord.RecordDelete), the one its request carried. The delete
-- writes it in the delete's own statement, and every broker replica reads it beside the records,
-- so each reads a record whose delete verifies as the routes' delete, and a record that is gone
-- with no such row as a database writer's.
create table nats_hold_record_deletes (
  hold_id text not null,
  kind text not null check (kind in ('cohort', 'convert', 'done', 'selection')),
  key text not null,
  gen bigint not null check (gen >= 1),
  deleted_at timestamptz not null,
  sig text not null,
  primary key (hold_id, kind, key)
);

-- nats_cutover is one row per family (session, dispatch, or daemon/<project key>), keyed by the
-- family and not by a hold: done once the hold's conversion converted it, rolled_back once a
-- rollback restored it, with the hold and the hold row's gen it was written under.
create table nats_cutover (
  family text primary key,
  state text not null check (state in ('done', 'rolled_back')),
  hold_id text not null,
  hold_gen bigint not null,
  written_at timestamptz not null,
  written_by text not null,
  sig text not null
);
