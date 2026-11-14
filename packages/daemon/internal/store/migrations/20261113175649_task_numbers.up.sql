-- 20261113175649_task_numbers.up.sql — a claim's task as a number the record writes and the
-- worker takes, rather than a prompt the daemon pushes and then judges by its acknowledgement
-- (INFRA-2811).
--
-- What stands before this migration is the push path's record of a pending task: three marks on
-- its pending_task_deliveries row saying what came of the prompts the daemon sent of it.
-- delivered_at is the acknowledgement of the latest prompt (0002_claims), marked_by the delivery
-- id that prompt carried (0018_delivery_marked_by), and prompted whether any prompt of the task
-- went out at all (20261029121000_claim_death_and_start_causes). A task the worker takes is
-- acknowledged by nothing: the take is the worker's own act, the daemon's answer to it is what
-- records the task as taken (confirmed_at), and the three marks have nothing left to say, so this
-- migration drops them.
--
-- The columns it adds are the record of that task as a number. claims.task_written is the highest
-- number the record queued, task_taken the number of the last take the daemon accepted,
-- task_finished the number of the last task whose turn finished, and task_finished_session the
-- claim's session when it finished it. task_seen_epoch and task_seen_revision are the bucket epoch
-- and revision of the claim's value that its worker last reported seeing, task_take_state what
-- came of a take at that revision ('' while none came, made, accepted or refused), and
-- task_written_epoch and task_written_revision the epoch and revision of the daemon's own last
-- write of the value. A revision is carried beside its epoch because a bucket created again counts
-- its revisions from 1 again. pending_task_deliveries.number is the number the pending task was
-- queued under, origin the source that queued it, and taken_text the text the accepted take of it
-- was answered with, null while no take has been accepted, so a take sent again after its answer
-- was lost is answered with that same text.
--
-- The fill numbers what the release before this one left as each claim's first task. A claim
-- holding a pending row holds task 1: the row is number 1 and the claim's task_written is 1, and
-- where the row is confirmed — the turn it was sent for started — its worker took it, so
-- task_taken is 1 as well. A claim holding no pending row that served a task, a run
-- (serving_generation <> 0) or a phase entry (served_entry > 0), took and finished task 1 in the
-- session it holds, so task_written, task_taken and task_finished are 1 and task_finished_session
-- is that session. Every other claim was given nothing and stays at 0 throughout. The epochs and
-- revisions start empty, because no value in the bucket is the record's yet, so the first boot
-- writes every owed value once. origin starts empty, which names no source: the task was queued
-- before the record kept one. taken_text starts null, since no take of such a task was ever
-- answered.
--
-- Once this migration has run, the daemon of the release before it cannot read the claims table
-- at all: its own claim read names d.delivered_at, d.marked_by and d.prompted
-- (packages/daemon/internal/store/claims.go, claimSelect), so every claim read fails and the
-- supervisor loads nothing, and its delivery write names all three (putDelivery). Rolling the image
-- back therefore needs the three columns back first. This directory keeps no down migrations —
-- migrations here are forward-only and no .down.sql file exists — so the way back is the seventeen
-- statements below, run with the daemon stopped. docs/kubernetes.md carries the same block as the
-- step an operator runs, beside the phase entry's, and the two say the same thing.
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   alter table pending_task_deliveries add column if not exists delivered_at timestamptz;
--   alter table pending_task_deliveries add column if not exists marked_by text not null default '';
--   alter table pending_task_deliveries add column if not exists prompted boolean not null default false;
--   update outbox set payload = payload - 'origin' where kind = 'supervise' and payload ? 'origin';
--   alter table claims drop column if exists task_written;
--   alter table claims drop column if exists task_taken;
--   alter table claims drop column if exists task_finished;
--   alter table claims drop column if exists task_finished_session;
--   alter table claims drop column if exists task_seen_epoch;
--   alter table claims drop column if exists task_seen_revision;
--   alter table claims drop column if exists task_take_state;
--   alter table claims drop column if exists task_written_epoch;
--   alter table claims drop column if exists task_written_revision;
--   alter table pending_task_deliveries drop column if exists number;
--   alter table pending_task_deliveries drop column if exists origin;
--   alter table pending_task_deliveries drop column if exists taken_text;
--   delete from schema_version where version = 20261113175649;
--   commit;
--   SQL
--
-- The run prints BEGIN, three ALTER TABLEs, UPDATE <n> for the supervise rows whose payload
-- carried the key, twelve ALTER TABLEs, DELETE 1 and COMMIT. A rerun prints a NOTICE for each of
-- the fifteen columns it finds as it means to leave them, the same ALTER TABLEs, UPDATE 0, DELETE 0
-- and COMMIT, and changes nothing. The transaction, the ON_ERROR_STOP and the if-exists clauses are
-- what make the seventeen one act that may be run twice, for the reason the phase entry's block
-- gives: run as bare statements in a psql without ON_ERROR_STOP, either half a failure leaves is a
-- broken store — the drops taken while the delete fails leave this release a schema it reads by
-- names that no longer stand, and the delete taken while a drop fails stops the roll-forward at
-- its first add column.
--
-- The first three statements are the columns that daemon reads and writes, back at the defaults
-- their own migrations gave them: no acknowledgement, no marking prompt and no prompt sent. That is
-- the record of a task the older daemon has queued and not yet sent, so it sends an unconfirmed
-- pending task at its claim's next ready as it sends any task it queued itself, and reads a
-- confirmed one as a turn under way.
--
-- The fourth is the payload key this release writes, taken back out, as the phase entry's fourth
-- takes out the two keys that release writes. This release writes origin into every supervise
-- start a source queues (record.SuperviseRequest.Origin); the release before it knows no such key
-- and decodes an outbox payload strictly (record.DecodeOutboxPayload, DisallowUnknownFields), and
-- its record.Postgres.RoleRuns decodes every supervise row of every issue an admission pass reads,
-- so a row carrying the key left in the table fails to decode for ever: the runner retries it on
-- its backoff and every admission pass of its issue fails with it. Nothing that daemon reads is
-- lost with the key, since it records no origin, and a row the roll-forward finds naming none is
-- read as a phase start where it names a phase and an operator's task otherwise (supervise's
-- originOf), as a row of the release before this one is.
--
-- The twelve drops and the deleted version row are for the roll-forward that follows the rollback
-- rather than for the older daemon, which reads none of the twelve columns and writes none of them:
-- its claim and delivery upserts name their own columns (putClaim, putDelivery), so a row it
-- inserts takes each new column's default and a row it updates keeps whatever the column held.
-- Left standing, those columns would be read on the roll-forward as current when nothing kept them
-- so: a task that daemon queued carries number 0 under a claim whose task_written names an older
-- task, and a task it finished leaves task_finished where it stood. Together the drops and the
-- delete re-arm this migration. store.apply decides each migration on its own row —
-- versionRecorded asks `select exists (select 1 from schema_version where version = $1::bigint)`
-- (internal/store/store.go) — so deleting this one row leaves this migration unapplied and every
-- other migration recorded and untouched, and when the newer image comes back its Migrate runs this
-- file again from its first statement. The twelve columns go with the row because those first
-- statements add them and `add column` on a column that already stands fails; the three marks the
-- block brought back are dropped again by this file's last statements. The missing row does not
-- move the number the state read reports while the older image runs (Store.SchemaVersionTx, the
-- highest row recorded): every database this release migrated records
-- 20261112180548_claim_leak_check_cause, that older daemon's own newest migration, below this
-- version, so the number reads 20261112180548 after the delete, which is the truth about the image
-- that is running.
--
-- What that costs is the numbering, as the phase entry's second fill costs its own: the second
-- fill reads what the older daemon left exactly as this fill reads what the release before this
-- one left, so every claim stands at task 1 again, or at 0, whatever number it stood at before the
-- rollback. Nothing reads a number across the rollback: the worker of the older release takes no
-- value, and the epochs and revisions start empty again, so the first boot after the roll-forward
-- writes every owed value once under the number the second fill gave it.
--
-- The re-arm asks nothing of the other rows. What it does need is that no migration recorded
-- after this one builds on the twelve columns it drops, and at this release none is recorded after
-- it. A rollback past the release before this one — to contract 87, across
-- 20261111225552_phase_entry — runs this block first and the phase entry's own block after it: the
-- release before the phase entry reads the three marks as the release before this one does, and
-- the phase entry's block leaves this migration's columns alone. The roll-forward after both then
-- applies the two migrations again in version order, the phase entry's first, so this fill reads
-- the served_entry the phase entry's second fill wrote.
alter table claims add column task_written bigint not null default 0;
alter table claims add column task_taken bigint not null default 0;
alter table claims add column task_finished bigint not null default 0;
alter table claims add column task_finished_session text not null default '';
alter table claims add column task_seen_epoch text not null default '';
alter table claims add column task_seen_revision bigint not null default 0;
alter table claims add column task_take_state text not null default ''
  check (task_take_state in ('', 'made', 'accepted', 'refused'));
alter table claims add column task_written_epoch text not null default '';
alter table claims add column task_written_revision bigint not null default 0;
alter table pending_task_deliveries add column number bigint not null default 0;
alter table pending_task_deliveries add column origin text not null default '';
alter table pending_task_deliveries add column taken_text text;

-- Every pending row is its claim's task 1, written; a confirmed one is taken as well.
update pending_task_deliveries set number = 1;
update claims c set task_written = 1, task_taken = case when d.confirmed_at is null then 0 else 1 end
from pending_task_deliveries d
where d.claim_token = c.token;

-- A claim holding no pending row that served a run or a phase entry took and finished task 1, in
-- the session it holds.
update claims c set task_written = 1, task_taken = 1, task_finished = 1, task_finished_session = c.session
where (c.serving_generation <> 0 or c.served_entry > 0)
  and not exists (select 1 from pending_task_deliveries d where d.claim_token = c.token);

alter table pending_task_deliveries drop column delivered_at;
alter table pending_task_deliveries drop column marked_by;
alter table pending_task_deliveries drop column prompted;
