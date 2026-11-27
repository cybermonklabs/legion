-- 20261117025904_artifact_versions_refs_forms_trigger.up.sql
-- Every document version any build writes owes its document an index with the forms
-- 20261117025900 adds (refs_forms_owed and refs_forms_owe_version): a build that predates them
-- indexes the version's text without them, which deletes the rows they wrote. An image or file
-- version carries no markdown and owes nothing, which is 0122's and 20261103205041's predicate.
-- A version this build writes with its rendering indexes its own text and its blocks in the same
-- transaction, which deletes the document's row (refs.replaceRefs). One it writes with no rendering
-- leaves its blocks to the block index, and the row stays owed until this build's index of the
-- document settles it. So only a version an older build writes, or one whose blocks this build has
-- not yet indexed, stays owed.
-- Its own migration, so the transaction holds artifact_versions' lock alone (0122's precedent).
create trigger artifact_versions_refs_forms_owed after insert on artifact_versions
  for each row when (new.markdown is not null) execute function refs_forms_owe_version();
