-- 20261126040300_comment_block_refs_decisions_forms_insert_trigger.up.sql
-- Every insert into comment_block_refs of an edge to the retired decisions document owes
-- refs_decisions_forms_owed (20261126040000) the comment whose block holds it, as 20261126040200
-- owes each document an insert into block_refs adds one to, for that migration's reasons: the
-- release before this one reads the Decisions page's link as the issue's retired document, and a
-- comment whose block names the issue as a bare key too keeps the issue's edge while that release
-- adds the document's, with no delete for the triggers of 20261117025908 to see.
-- A statement trigger over the inserted rows, whose insert keeps an owed row's enqueued_at, and
-- the owner exists, since comment_block_refs.comment_id references it (20261126040200's rules).
-- Its own migration, so the transaction holds comment_block_refs' lock alone
-- (20261117025908's precedent).
create or replace function refs_decisions_forms_owe_inserted_comment_block_refs() returns trigger language plpgsql as $$
begin
  insert into refs_decisions_forms_owed (kind, id)
  select distinct 'comment', fresh.comment_id from fresh
  -- kept for one release: decisions-ref-repair
  where fresh.to_kind = 'artifact' and fresh.to_id like '%/decisions-md'
  on conflict (kind, id) do nothing;
  return null;
end;
$$;
create trigger comment_block_refs_decisions_forms_owed_insert after insert on comment_block_refs
  referencing new table as fresh
  for each statement execute function refs_decisions_forms_owe_inserted_comment_block_refs();
