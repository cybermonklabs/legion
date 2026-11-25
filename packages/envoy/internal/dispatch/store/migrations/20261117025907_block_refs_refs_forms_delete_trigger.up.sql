-- 20261117025907_block_refs_refs_forms_delete_trigger.up.sql
-- Every delete from block_refs owes refs_forms_owed (20261117025900) each document whose block
-- edges it deleted, as 20261117025906 owes each source a delete from refs touches. A build that
-- predates the forms that migration names re-indexes a document's blocks with the grammar it knows
-- and deletes the block edges of those forms, and such a re-index need write no version
-- (20261117025904) and delete no refs row (20261117025906): its block-index pass re-indexes a
-- document no write changed, and a text that names one issue twice, as a bare key in one block and
-- as a tab address in another (`CORE-1` ... `dispatch://CORE-1/plan`), keeps its refs edge, which
-- the bare key still finds, while the block holding the tab address loses its own.
-- This build re-indexes a document's blocks only beside its references, and settles the row after
-- both deletes, in the same transaction (refs.replaceRefs, after refs.IndexBlocks), so only a
-- delete by a build that does not settle this table stays owed.
--
-- A statement trigger over the deleted rows, whose insert keeps an owed row's enqueued_at (`do
-- nothing`), for 20261117025906's reasons. A delete that cascades from the document's own delete
-- owes nothing: the document is gone when the statement's trigger runs, and a gone document owes
-- no text.
-- Its own migration, so the transaction holds block_refs' lock alone, for milliseconds (0153's
-- precedent).
create or replace function refs_forms_owe_deleted_block_refs() returns trigger language plpgsql as $$
begin
  insert into refs_forms_owed (kind, id)
  select distinct 'artifact', gone.artifact_id from gone
  where exists (select 1 from artifacts a where a.id = gone.artifact_id)
  on conflict (kind, id) do nothing;
  return null;
end;
$$;
create trigger block_refs_refs_forms_owed_delete after delete on block_refs
  referencing old table as gone
  for each statement execute function refs_forms_owe_deleted_block_refs();
