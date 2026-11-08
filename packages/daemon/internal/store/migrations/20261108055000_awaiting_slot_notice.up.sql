-- 20261108055000_awaiting_slot_notice.up.sql -- the one notice that tells a root's architect that
-- its answered root waits for a slot.
--
-- awaiting_slot_notice_at is when the daemon wrote that architect the waiting-for-slot notice for
-- the wait the row holds: an admission pass writes it when it finds the wait answered, the root
-- needing an admission slot and the cap holding none free. It records that the notice was
-- written, not that an architect read it, and it is the mark that holds the daemon to one notice
-- per wait. The column is the wait's own, as every awaiting_ column is: a write that leaves
-- awaiting_person clears it with the rest of the wait, and the shape check below refuses it in
-- every other phase.
--
-- The check also refuses the mark on a wait no act has answered (awaiting_answered_at is null).
-- Nothing of a wait still with a person is told to wait for a slot: such a root wants no slot
-- yet. Postgres has no `alter constraint`, so the whole of issues_awaiting_shape (0057) is
-- dropped and added again with the two new clauses, as 0049 and 0057 already do for
-- issues_phase_check.
--
-- There is no backfill, and the empty column is the point: the first admission pass after this
-- deploy reads every root already answered and waiting as one it has told nobody about, and tells
-- each of their architects once. A backfill of now() would silence that pass, and the architects
-- waiting at the deploy are exactly the ones with nothing yet to read.
--
-- Rolling the image back costs nothing: an earlier daemon's write of an issue names no such
-- column, so the row keeps the mark it holds, and no earlier release reads it. Migrations here
-- are forward-only and this one has no down file.
alter table issues add column awaiting_slot_notice_at timestamptz;
alter table issues drop constraint issues_awaiting_shape;
alter table issues add constraint issues_awaiting_shape check (
  case when phase = 'awaiting_person'
    then (awaiting_ask is null) <> (awaiting_gate_version is null) and held_from is null
      and (awaiting_gate_version is null or awaiting_artifact is not null)
      and (awaiting_answered_by is null or awaiting_answered_at is not null)
      and (awaiting_answered_detail is null or awaiting_answered_by is not null)
      and (awaiting_slot_notice_at is null or awaiting_answered_at is not null)
    else awaiting_ask is null and awaiting_gate_version is null and awaiting_artifact is null
      and awaiting_answered_at is null and awaiting_answered_by is null and awaiting_answered_detail is null
      and awaiting_since_seq is null and awaiting_since is null
      and awaiting_slot_notice_at is null
  end);
