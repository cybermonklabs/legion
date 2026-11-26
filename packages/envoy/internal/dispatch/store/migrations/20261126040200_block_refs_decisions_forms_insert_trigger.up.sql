-- 20261126040200_block_refs_decisions_forms_insert_trigger.up.sql
-- Every insert into block_refs of an edge to the retired decisions document owes
-- refs_decisions_forms_owed (20261126040000) the document whose block holds it.
-- The release before this one reads the Decisions page's link, `/issues/CORE-45/decisions`, as the
-- issue's former decisions document, where this release reads it as the issue. When it
-- re-indexes a document's blocks it deletes the issue's edge the link's block
-- holds.
-- That owes refs_forms_owed (20261117025907) and, once that release settles the row,
-- refs_decisions_forms_owed (20261126040100). A block that also names the issue as a bare key
-- loses nothing: the issue's edge is the bare key's too, and the release before only adds the
-- retired document's edge to the block, and to refs. No delete is made, so no trigger above owes
-- anything, though the block now holds an edge this release's index of the same text does not
-- write. The retired document's edge is the one edge such a re-index inserts that this release
-- does not, so it is the one this trigger reads: a row whose target is an artifact whose key ends
-- in the old document's slug (refs.RetiredDecisionsSlug). An explicit link to that document
-- writes the same edge in this build: a write that indexes
-- the document's whole text then clears the row this trigger wrote, in a statement after every
-- insert of its blocks (refs.queueSettleSourceOwed), and a re-index this build makes of a text
-- that holds such a link costs one more pass of the drain, once.
-- No trigger on refs of its own: the release before writes an edge to refs alone only in its
-- rebuild-refs and for a document version with no rendering, each of which leaves the document
-- owed a block index, and its block-index pass then inserts the edge into block_refs, where this
-- trigger sees it.
--
-- A statement trigger over the inserted rows, whose insert keeps an owed row's enqueued_at (`do
-- nothing`), for 20261126040100's reasons. A row an upsert finds already present is not inserted
-- and owes nothing, and the owner exists, since block_refs.artifact_id references it.
-- Its own migration, so the transaction holds block_refs' lock alone, for milliseconds
-- (20261117025907's precedent).
create or replace function refs_decisions_forms_owe_inserted_block_refs() returns trigger language plpgsql as $$
begin
  insert into refs_decisions_forms_owed (kind, id)
  select distinct 'artifact', fresh.artifact_id from fresh
  -- kept for one release: decisions-ref-repair
  where fresh.to_kind = 'artifact' and fresh.to_id like '%/decisions-md'
  on conflict (kind, id) do nothing;
  return null;
end;
$$;
create trigger block_refs_decisions_forms_owed_insert after insert on block_refs
  referencing new table as fresh
  for each statement execute function refs_decisions_forms_owe_inserted_block_refs();
