-- Run by hand outside a transaction, before rolling back past 20261125120102. Nothing depends on
-- the index; without it the removal's cascade through user_document_reads_artifact_id_fkey reads
-- user_document_reads whole again.
drop index concurrently if exists user_document_reads_artifact_id;
