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
-- deploy reads every root already answered and waiting as one it has told nobody about, and
-- tells each architect that can hear it — one whose claim holds its Envoy role — once, leaving
-- the rest unmarked for the pass after their architects take theirs. A backfill of now() would
-- silence that pass, and the architects waiting at the deploy are exactly the ones with nothing
-- yet to read.
--
-- Rolling the image back leaves every mark where it stands, and the trigger below is what makes
-- that safe. An earlier daemon's PutIssue upsert names no such column, so on conflict a marked
-- row keeps its mark, and that daemon's next write of such a root would stand the mark where the
-- check forbids it: its resume (phase admitted, the wait cleared), the linger or the
-- re-admission that clears the wait, and a new wait written over the old one each write every
-- awaiting column but this one. Admission runs inside every fact's transaction, so one such row
-- would fail every fact of its project — each Dispatch event, each tick, each boot pass — until
-- a person repaired it by hand.
--
-- issues_carried_slot_notice_cleared clears a mark a write carries forward unchanged into a row
-- that is no longer an answered wait, whichever build writes the row, as 0020's
-- issues_hold_reason_follows_hold clears a hold's reason on any write that leaves the issue
-- unheld. So an earlier daemon's writes land, and the next wait it starts inherits nothing of
-- the telling of the old one. A write that states the mark itself is left for the check, which
-- refuses it outside an answered wait: this release writes the mark in one place
-- (admit.tellWaitingForSlot), on a row that is an answered wait, and the check stays what holds
-- that release to it.
--
-- What a rollback costs is the notice nobody writes. The earlier release has no such notice at
-- all, so a root whose wait it answers waits with its architect told nothing; and a root told
-- before the rollback keeps its mark across it, so the first pass after the roll forward tells
-- that architect nothing again, the mark still saying its wait was told. Migrations here are
-- forward-only and this one has no down file.
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

create function issues_carried_slot_notice_cleared() returns trigger language plpgsql as $$
begin
  new.awaiting_slot_notice_at := null;
  return new;
end $$;

-- The condition is the carried mark exactly: a row that held one, a write that names no other
-- value for it, and a row left standing in something other than an answered wait. An insert
-- carries nothing, so it is not here: a row inserted with a mark states it, and the check reads
-- it.
create trigger issues_carried_slot_notice_cleared before update on issues
  for each row
  when (old.awaiting_slot_notice_at is not null
    and new.awaiting_slot_notice_at is not distinct from old.awaiting_slot_notice_at
    and not (new.phase = 'awaiting_person' and new.awaiting_answered_at is not null))
  execute function issues_carried_slot_notice_cleared();
