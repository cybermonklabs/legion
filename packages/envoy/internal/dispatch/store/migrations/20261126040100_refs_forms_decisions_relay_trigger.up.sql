-- 20261126040100_refs_forms_decisions_relay_trigger.up.sql
-- Every delete from refs_forms_owed (20261117025900) relays each row it removes into
-- refs_decisions_forms_owed (20261126040000), whoever sends the delete.
-- The release before this one settles a row of refs_forms_owed as soon as it has re-indexed the
-- text with the grammar it knows (refs.queueSettleSourceOwed and ItemIndexing.queueRecordIndexed
-- in that release), which is a delete from that table. A text holding `dispatch://CORE-45/decisions`
-- or the Decisions page's link, which that build reads as no reference and as the issue's retired
-- document, then has the issue's edge deleted and its row of refs_forms_owed deleted in the same
-- transaction, so nothing says the text was re-indexed without the edge. The relay is what does:
-- the row that build deletes is owed here, until a build that reads the page has indexed the text
-- itself.
-- This release deletes from refs_forms_owed too, in every transaction that indexes a source's
-- whole text, and clears the row the relay wrote in a statement of its own after that delete
-- (refs.queueSettleSourceOwed, ItemIndexing.queueRecordIndexed, refs.ForgetDocument): a statement
-- trigger fires at the end of its statement, so a delete of the relayed row in the statement that
-- relays it would run before the relay and leave the row for good.
--
-- A statement trigger over the deleted rows rather than a row trigger, as 20261117025906's: a
-- write settles one row, and the orphan sweep thousands, and the statement inserts once. A
-- statement that deletes no row inserts nothing.
-- Only the rows of a source that still exists are relayed, by the predicate of the orphan sweep
-- (refs.sweepOrphans: an issue message needs its issue), as 20261117025907-20261117025910 owe only
-- an owner that exists: a gone source owes no text, and the sweep's own delete from
-- refs_forms_owed relays nothing. The removal of a document deletes its row of refs_forms_owed
-- before the document's own row (refs.ForgetDocument, in the release before too), so the relay
-- writes a row for it that this release clears in the same transaction and the release before
-- leaves, which the drain settles as a document that is gone.
-- The insert keeps an owed row's enqueued_at (`do nothing`): a delete re-reads no text, it only
-- says the text is owed, and every path that sends it holds the row its source's writers hold (the
-- source row, or a document's owner row), so a drain that listed the row earlier either ran after
-- this delete committed and indexes the text as it stands, or ran before it and settled the row,
-- which this insert then writes again (20261117025906's reasoning).
--
-- Its own migration, so the transaction holds refs_forms_owed's lock alone: create trigger takes
-- SHARE ROW EXCLUSIVE, which waits for every writer of the table and holds each one's write back
-- until it commits, for milliseconds. Both builds' writes of a text insert into that table
-- (20261117025901-20261117025904), so the wait is bounded by the runner's lock timeout and tried
-- again, as every trigger migration's is. It touches no row.
create or replace function refs_decisions_forms_relay() returns trigger language plpgsql as $$
begin
  insert into refs_decisions_forms_owed (kind, id)
  select gone.kind, gone.id from gone
  where exists (
    select 1 from artifacts a where gone.kind = 'artifact' and a.id = gone.id
    union all
    select 1 from asks k where gone.kind = 'ask' and k.id = gone.id
    union all
    select 1 from comments c where gone.kind = 'comment' and c.id = gone.id
    union all
    select 1 from messages m where gone.kind = 'message' and m.issue_key is not null and m.id = gone.id
  )
  on conflict (kind, id) do nothing;
  return null;
end;
$$;
create trigger refs_forms_owed_decisions_relay after delete on refs_forms_owed
  referencing old table as gone
  for each statement execute function refs_decisions_forms_relay();
