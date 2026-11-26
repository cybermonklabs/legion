-- 20261117025910_ask_block_refs_refs_forms_delete_trigger.up.sql
-- Every delete from ask_block_refs owes refs_forms_owed (20261117025900) each ask whose part edges
-- it deleted, as 20261117025908 owes each comment, for that migration's reasons. A build that
-- predates the forms 20261117025900 names re-indexes an ask's parts - its question, its options and
-- its answer (refs.AskParts) - with the grammar it knows on every transition of the ask, an answer
-- and a resolution alike (api's closeAskTx), and deletes the part edges of those forms. A
-- resolution writes no column the asks trigger watches (20261117025902), and where the question
-- names the same target elsewhere no refs row goes with them, so this delete is all such a
-- re-index leaves to see. This build re-indexes an ask's parts only beside its question and settles
-- the row after both (refs.IndexAskParts), so only a delete by a build that does not settle this
-- table stays owed.
-- A statement trigger over the deleted rows, whose insert keeps an owed row's enqueued_at, and a
-- delete that cascades from the ask's own delete owes nothing (20261117025907's rules).
-- Its own migration, so the transaction holds ask_block_refs' lock alone (0153's precedent).
create or replace function refs_forms_owe_deleted_ask_block_refs() returns trigger language plpgsql as $$
begin
  insert into refs_forms_owed (kind, id)
  select distinct 'ask', gone.ask_id from gone
  where exists (select 1 from asks k where k.id = gone.ask_id)
  on conflict (kind, id) do nothing;
  return null;
end;
$$;
create trigger ask_block_refs_refs_forms_owed_delete after delete on ask_block_refs
  referencing old table as gone
  for each statement execute function refs_forms_owe_deleted_ask_block_refs();
