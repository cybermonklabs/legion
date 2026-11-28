-- 20261126040500_ask_block_refs_decisions_forms_insert_trigger.up.sql
-- Every insert into ask_block_refs of an edge to the retired decisions document owes
-- refs_decisions_forms_owed (20261126040000) the ask whose part holds it, as 20261126040300 owes
-- each comment, for that migration's reasons. An ask's own refs row holds the links of its
-- question alone (refs.AskParts): a link in an option or in the answer is in ask_block_refs and
-- nowhere else, so the release before this one, which reads the Decisions page's link as the
-- issue's retired document, leaves no other trace of it there.
-- A statement trigger over the inserted rows, whose insert keeps an owed row's enqueued_at, and the
-- owner exists, since ask_block_refs.ask_id references it (20261126040200's rules).
-- Its own migration, so the transaction holds ask_block_refs' lock alone (20261117025910's
-- precedent).
create or replace function refs_decisions_forms_owe_inserted_ask_block_refs() returns trigger language plpgsql as $$
begin
  insert into refs_decisions_forms_owed (kind, id)
  select distinct 'ask', fresh.ask_id from fresh
  -- kept for one release: decisions-ref-repair
  where fresh.to_kind = 'artifact' and fresh.to_id like '%/decisions-md'
  on conflict (kind, id) do nothing;
  return null;
end;
$$;
create trigger ask_block_refs_decisions_forms_owed_insert after insert on ask_block_refs
  referencing new table as fresh
  for each statement execute function refs_decisions_forms_owe_inserted_ask_block_refs();
