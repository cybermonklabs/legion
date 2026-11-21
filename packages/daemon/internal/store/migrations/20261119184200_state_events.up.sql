-- 20261119184200_state_events.up.sql — the daemon's state events: the three outbox kinds that
-- carry them (state_change, state_admission, state_boot), the counter that gives each row its
-- sequence and its time when its transaction commits, and the admission block the daemon last
-- wrote (dispatch://INFRA-3931).
--
-- The daemon writes a state event as an outbox row in the transaction of the write it reports:
-- a state_change when a write moves an issue's phase, tree, slot, waitingOn or claim, a
-- state_admission when the admission block changes, and a state_boot at each boot
-- (internal/record's StateChange, StateAdmission and StateBoot). Dispatch reads the events in
-- sequence order and takes the daemon's whole state again when it finds a gap, so the sequence
-- has to follow commit order and have no gap. Every state row carries the bare Dispatch project
-- key in outbox.issue, as gated_steps_publish does, so a project has one state lane, and that
-- lane publishes its rows strictly in sequence order (internal/record's outboxInOrder).
--
-- The check is one list, so the migration that sets it names every kind. This one follows the list
-- of 20261119003157: that migration's twenty-nine kinds, conflict_confirm, merge_forward,
-- step_record and plan_record among them, and these three, thirty-two in all. Its version stands
-- above 20261119003157 on purpose: Migrate applies every unrecorded migration in version order, so
-- the list with the higher version is the last word on any database. A version below
-- 20261119003157 would let that migration run after this one on a fresh database and write its
-- list without the three state kinds, and the check would then refuse every state row. A list that
-- left out one of 20261119003157's kinds would refuse every row of that kind the daemon queues, and
-- fail the upgrade of a database that holds one pending.
--
-- phase_ended stays in the list for the reason 20261119003157 keeps it. The daemon no longer
-- queues a row of it, but the release before 20261119003157 does. After a rollback that release
-- runs on the check this file writes, since it has no migration of its own left to apply, and a
-- list without phase_ended would refuse the first such row it queues (SQLSTATE 23514).
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'state_change', 'state_admission', 'state_boot'));

-- One counter row per project. last_seq is the sequence of the newest state row the project's
-- daemon committed, last_at that row's time, and last_tx the transaction that committed it. The
-- state route reads last_seq in the snapshot it serves, so a reader knows which events that
-- snapshot holds: every event at or below it, and none above it. store_id is the id of this
-- store's count: the commit trigger's first insert of the project's row makes it, and no statement
-- writes it after. The state route serves it as daemon.stateId and every state message carries it,
-- so a reader applies only messages counted in the store its starting point was read from: a store
-- made afresh behind the same daemon makes a new id, and its sequences, which start again at 1, are
-- never read as the next ones of the old count.
create table state_publication (
  project text primary key,
  last_seq bigint not null default 0,
  last_at timestamptz,
  last_tx bigint,
  store_id uuid not null default gen_random_uuid()
);

-- The admission block the daemon last wrote for the project. The admission pass compares its new
-- block with this one, and writes a state_admission row only when the two differ. The block is in
-- a table of its own so that the pass's write does not lock the counter row: only the commit
-- trigger below takes that lock.
create table state_admission_blocks (
  project text primary key,
  block jsonb not null
);

-- A state row's sequence, its time and the id of the transaction that committed it. The trigger
-- below writes them when that transaction commits. They are null on every row of another kind,
-- and on a state row until its transaction commits. The unique index holds each project to one
-- row per sequence, and it is what the lane rule reads to find a project's lowest unfinished
-- state row.
alter table outbox
  add column state_seq bigint,
  add column state_at timestamptz,
  add column state_tx bigint;

create unique index outbox_state_seq_unique
  on outbox (issue, state_seq)
  where state_seq is not null;

-- The commit trigger. It is deferred, so it runs at the commit of the transaction that inserted
-- the row, after every statement of that transaction. It steps the project's counter (the row's
-- issue column, which holds the bare project key) and writes the new values onto the row:
--
-- * last_seq is the previous value plus one, or 1 for the project's first state row.
-- * last_at is the same value for every row of one transaction. The transaction's first row takes
--   the later of the clock and the previous row's time plus one microsecond, so the time never
--   goes backwards, even when the clock does. A later row of the same transaction finds last_tx
--   equal to its own transaction's id and keeps last_at.
-- * last_tx is the id of the committing transaction.
--
-- The counter row's lock is taken here, at the commit, and released when the commit ends. A second
-- transaction that commits a state row of the same project at the same time waits for that lock,
-- and then steps the counter from the first transaction's value. So the sequence follows commit
-- order. A transaction that rolls back never runs the trigger, so it leaves no step and no gap.
-- Nothing in the commit takes a lock after this one (no other deferred trigger or constraint
-- exists in the schema), so two transactions wait on each other only for a commit and never
-- deadlock here. The upsert needs READ COMMITTED, which every daemon write uses: under REPEATABLE
-- READ, a counter step that another transaction committed first would fail the commit with a
-- serialization error.
--
-- A row that its own transaction deleted before the commit takes no step, so it leaves no gap.
--
-- The update of the outbox row fires no trigger: the outbox has no update trigger, and this one
-- fires on insert. The update changes only the three state columns, so the only constraint it
-- can meet is the unique index above, which the counter's one step per row satisfies.
create function outbox_state_sequence() returns trigger language plpgsql as $$
declare
  taken state_publication%rowtype;
begin
  perform 1 from outbox where id = new.id;
  if not found then
    return null;
  end if;
  insert into state_publication as counter (project, last_seq, last_at, last_tx)
    values (new.issue, 1, clock_timestamp(), txid_current())
  on conflict (project) do update set
    last_seq = counter.last_seq + 1,
    last_at = case
      when counter.last_tx = txid_current() then counter.last_at
      else greatest(clock_timestamp(), counter.last_at + interval '1 microsecond')
    end,
    last_tx = txid_current()
  returning * into taken;
  update outbox set state_seq = taken.last_seq, state_at = taken.last_at, state_tx = taken.last_tx
    where id = new.id;
  return null;
end $$;

create constraint trigger outbox_state_sequence
  after insert on outbox
  deferrable initially deferred
  for each row when (new.kind in ('state_change', 'state_admission', 'state_boot'))
  execute function outbox_state_sequence();
