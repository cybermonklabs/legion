-- 20261117025908_comment_block_refs_refs_forms_delete_trigger.up.sql
-- Every delete from comment_block_refs owes refs_forms_owed (20261117025900) each comment whose
-- block edges it deleted, as 20261117025907 owes each document a delete from block_refs touches,
-- for that migration's reasons: a build that predates the forms 20261117025900 names re-indexes a
-- comment's blocks with the grammar it knows - its block-index pass of a comment left owed one, its
-- drain of a `scope` row - and deletes the block edges of those forms, and where the comment names
-- the same target elsewhere no refs row goes with them. This build re-indexes a comment's blocks
-- only beside its references and settles the row after both (refs.replaceRefs, after
-- refs.IndexItem), so only a delete by a build that does not settle this table stays owed.
-- A statement trigger over the deleted rows, whose insert keeps an owed row's enqueued_at, and a
-- delete that cascades from the comment's own delete owes nothing (20261117025907's rules).
-- Its own migration, so the transaction holds comment_block_refs' lock alone (0153's precedent).
create or replace function refs_forms_owe_deleted_comment_block_refs() returns trigger language plpgsql as $$
begin
  insert into refs_forms_owed (kind, id)
  select distinct 'comment', gone.comment_id from gone
  where exists (select 1 from comments c where c.id = gone.comment_id)
  on conflict (kind, id) do nothing;
  return null;
end;
$$;
create trigger comment_block_refs_refs_forms_owed_delete after delete on comment_block_refs
  referencing old table as gone
  for each statement execute function refs_forms_owe_deleted_comment_block_refs();
