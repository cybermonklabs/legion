-- 20261117025909_message_block_refs_refs_forms_delete_trigger.up.sql
-- Every delete from message_block_refs owes refs_forms_owed (20261117025900) each issue message
-- whose block edges it deleted, as 20261117025908 owes each comment, for that migration's reasons.
-- A direct message holds no block edge and is indexed by nothing, so only a message with an issue
-- is owed, the messages 20261117025903 and the seed owe. This build re-indexes a message's blocks
-- only beside its references and settles the row after both (refs.replaceRefs, after
-- refs.IndexItem), so only a delete by a build that does not settle this table stays owed.
-- A statement trigger over the deleted rows, whose insert keeps an owed row's enqueued_at, and a
-- delete that cascades from the message's own delete owes nothing (20261117025907's rules).
-- Its own migration, so the transaction holds message_block_refs' lock alone (0153's precedent).
create or replace function refs_forms_owe_deleted_message_block_refs() returns trigger language plpgsql as $$
begin
  insert into refs_forms_owed (kind, id)
  select distinct 'message', gone.message_id from gone
  where exists (select 1 from messages m where m.id = gone.message_id and m.issue_key is not null)
  on conflict (kind, id) do nothing;
  return null;
end;
$$;
create trigger message_block_refs_refs_forms_owed_delete after delete on message_block_refs
  referencing old table as gone
  for each statement execute function refs_forms_owe_deleted_message_block_refs();
