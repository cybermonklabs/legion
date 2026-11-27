-- Run by hand outside a transaction, before rolling back past 20261125120101. Nothing depends on
-- the index; without it the removal's cascade through related_checks_source_artifact_id_fkey reads
-- related_checks whole again.
drop index concurrently if exists related_checks_source_artifact_id;
