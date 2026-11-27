-- 20261122100124_document_block_identities_owed.up.sql
--
-- Whether the store states the identity mentions of a document's blocks:
--   document_block_versions.identities_stated   false where a build before this release stated
--                                                the document's blocks in the node and tuple
--                                                store (store_version) without the mentions of a
--                                                person, an agent's session or a role a block
--                                                names, true where this release's pass did.
--
-- This release's block index states, beside each block's node, part_of and links, a `mentions`
-- tuple out of the block to each identity its text names (refs' blockIdentityMentions, R5). The
-- store's own release states the rest of a block and none of these, and it writes the same
-- revision fence (store_version) this release's pass writes, so a document it indexed stands at
-- its version with no mark, and no pass of this release reads it again: its blocks would name a
-- person and mention nobody for good. The column says which documents those are. Every row there
-- before this migration was written by such a build, and holds false (the column's first
-- default); a row this release inserts holds true (its default from then on); and the trigger
-- below gives false to every row a build before this release inserts or updates. The backfill and
-- the block-index tick's catch-up read the rows that hold false and stand at a version
-- (graphcensus' blockIdentitiesPass): each that holds a block whose text names an identity is
-- marked owed this release's pass (store_owed_at, as the backfill marks a document), which states
-- the mentions at the version the row stands at and appends none that stands already, and every
-- row read is set true. The pass that drains the mark writes the fence on this release's
-- connection, which the trigger passes by, so the row stays true; a build before this release
-- that drains it first writes false again, and the next catch-up marks it again.
--
-- The trigger's WHEN is the marker triggers' own (20261116150000, 20261122100122): every
-- connection of this release's pool carries dispatch.store_marker_writer `on`
-- (store.StoreMarkerWriterSetting), so this release's writes of the row never call the function,
-- and the function keeps the same test, so a trigger that calls it without the WHEN still marks
-- only an older build's write. It changes only the row the write writes and reads no table.
--
-- The added column holds a constant default, so Postgres records it in the catalog and rewrites
-- no row, and the second default is a catalog write too. Both `alter table` statements take
-- ACCESS EXCLUSIVE on document_block_versions, and `create trigger` SHARE ROW EXCLUSIVE, each held
-- until the migration commits, at once: no statement of the migration waits on anything after
-- them. Half a second bounds the wait for the lock, as 20261122100122 bounds its own: past it the
-- migration fails with 55P03 having applied nothing, and the runner applies it again
-- (pgmigrate.LockTimeoutAttempts). The release before this one never reads the column; rolling
-- back past this migration is an image rollback alone, after which every row that build writes
-- holds false until this release serves again and its catch-up reads it.
set local lock_timeout = '500ms';

-- kept for one release: block-identities-stated
alter table document_block_versions add column identities_stated boolean not null default false;
-- kept for one release: block-identities-stated
alter table document_block_versions alter column identities_stated set default true;

-- kept for one release: block-identities-stated
create function document_block_versions_identities_by_older_writer() returns trigger language plpgsql as $$
begin
  if current_setting('dispatch.store_marker_writer', true) is distinct from 'on' then
    new.identities_stated := false;
  end if;
  return new;
end;
$$;

-- kept for one release: block-identities-stated
create trigger document_block_versions_identities before insert or update on document_block_versions
  for each row
  when (current_setting('dispatch.store_marker_writer', true) is distinct from 'on')
  execute function document_block_versions_identities_by_older_writer();
