-- 20261126040400_message_block_refs_decisions_forms_insert_trigger.up.sql
-- Every insert into message_block_refs of an edge to the retired decisions document owes
-- refs_decisions_forms_owed (20261126040000) the issue message whose block holds it, as
-- 20261126040300 owes each comment, for that migration's reasons. A direct message holds no block
-- edge and is indexed by nothing, so only an issue message is owed: message_block_refs.message_id
-- references the message, and the release before indexes the blocks of an issue message alone.
-- A statement trigger over the inserted rows, whose insert keeps an owed row's enqueued_at
-- (20261126040200's rules).
-- Its own migration, so the transaction holds message_block_refs' lock alone
-- (20261117025909's precedent).
create or replace function refs_decisions_forms_owe_inserted_message_block_refs() returns trigger language plpgsql as $$
begin
  insert into refs_decisions_forms_owed (kind, id)
  select distinct 'message', fresh.message_id from fresh
  -- kept for one release: decisions-ref-repair
  where fresh.to_kind = 'artifact' and fresh.to_id like '%/decisions-md'
    and exists (select 1 from messages m where m.id = fresh.message_id and m.issue_key is not null)
  on conflict (kind, id) do nothing;
  return null;
end;
$$;
create trigger message_block_refs_decisions_forms_owed_insert after insert on message_block_refs
  referencing new table as fresh
  for each statement execute function refs_decisions_forms_owe_inserted_message_block_refs();
