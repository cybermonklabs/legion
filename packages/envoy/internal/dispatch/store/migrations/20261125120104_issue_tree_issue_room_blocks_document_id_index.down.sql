-- Run by hand outside a transaction, before rolling back past 20261125120104. Nothing depends on
-- the index; without it the removal's check of issue_room_blocks_document_id_fkey reads
-- issue_room_blocks whole again.
drop index concurrently if exists issue_room_blocks_document_id;
