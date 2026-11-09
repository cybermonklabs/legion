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
-- issues_carried_slot_notice_cleared clears any mark a write leaves unchanged while it leaves the
-- row standing in something other than an answered wait, whichever release writes that row, as
-- 0020's issues_hold_reason_follows_hold clears a hold's reason on any write that leaves the
-- issue unheld. Unchanged is the whole of what the condition can read: it compares the new value
-- with the old one, and no trigger can tell a column a write named from one it left alone. This
-- release's PutIssue names awaiting_slot_notice_at in its insert and in its on-conflict update,
-- so every write of a marked row restates the struct's value, and a write of this release's that
-- carried a marked root out of its answered wait with the mark still in the struct would be
-- cleared here, silently, rather than refused by the check. No path does that: each one builds a
-- fresh Awaiting or sets it to nil, which record.Awaiting's own comment lists one by one. So an
-- earlier daemon's writes land, this release's land, and the next wait, whichever release starts
-- it, inherits nothing of the telling of the old one.
--
-- What the check is left to hold is a mark stated anew outside an answered wait — a value the
-- row did not hold before this write — which no release writes and the check refuses.
--
-- What a rollback costs is the notice nobody writes. The earlier release has no such notice at
-- all, so a root whose wait it answers waits with its architect told nothing; and a root told
-- before the rollback keeps its mark across it, so the first pass after the roll forward tells
-- that architect nothing again, the mark still saying its wait was told. It costs one operator
-- step as well, which is not this file's: a waiting-for-slot row still in the outbox is a notice
-- kind the earlier release cannot decode, and docs/kubernetes.md, under "The outbox rows the
-- earlier release refuses must go", carries the statement that deletes it and says when to run
-- it. Migrations here are forward-only and this one has no down file.
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

-- The condition is the mark a write leaves unchanged: a row that held one, a write that leaves
-- that same value standing, whether by naming it again or by not naming the column at all, and a
-- row left in something other than an answered wait. An insert is not here: it carries nothing
-- forward, so a row inserted with a mark states it, and the check reads it.
create trigger issues_carried_slot_notice_cleared before update on issues
  for each row
  when (old.awaiting_slot_notice_at is not null
    and new.awaiting_slot_notice_at is not distinct from old.awaiting_slot_notice_at
    and not (new.phase = 'awaiting_person' and new.awaiting_answered_at is not null))
  execute function issues_carried_slot_notice_cleared();
