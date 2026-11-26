-- 20261125005343_check_states_owe_document_blocks.up.sql
-- A check block of a Verification document that the block index inserts, changes or deletes marks
-- the check states of the document's issue owed (check_states_owe_check_block, 20261125005338):
-- the results read takes each check's words, version and digest from document_blocks, so a
-- change of a check's words is a write here, whichever build or reconciliation indexed it. The
-- update trigger fires only where the type, the digest or the changed version moved: the index
-- rewrites a row it finds unchanged too, and a mark for that would settle nothing. Dispatch
-- refuses to remove a document that plays a role (api/artifact_removal.go), so no cascade deletes
-- a Verification document's blocks past the delete trigger. Its own migration, so the transaction
-- holds document_blocks' SHARE ROW EXCLUSIVE alone, for the catalog write.
create trigger check_states_owe_check_block_insert after insert on document_blocks
  for each row when (new.type = 'check')
  execute function check_states_owe_check_block();
create trigger check_states_owe_check_block_update after update on document_blocks
  for each row when ((old.type = 'check' or new.type = 'check')
    and (old.type, old.text_sha256, old.changed_version) is distinct from (new.type, new.text_sha256, new.changed_version))
  execute function check_states_owe_check_block();
create trigger check_states_owe_check_block_delete after delete on document_blocks
  for each row when (old.type = 'check')
  execute function check_states_owe_check_block();
