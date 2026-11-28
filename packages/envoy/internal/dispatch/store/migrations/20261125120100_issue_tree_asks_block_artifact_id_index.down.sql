-- Run by hand outside a transaction, before rolling back past 20261125120100. Nothing depends on
-- the index; without it the removal's check of asks_block_artifact_id_fkey reads asks whole again.
drop index concurrently if exists asks_block_artifact_id;
