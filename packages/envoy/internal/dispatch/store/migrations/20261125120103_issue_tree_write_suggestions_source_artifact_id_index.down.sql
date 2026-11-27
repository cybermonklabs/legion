-- Run by hand outside a transaction, before rolling back past 20261125120103. Nothing depends on
-- the index; without it the removal's cascade through write_suggestions_source_artifact_id_fkey
-- reads write_suggestions whole again.
drop index concurrently if exists write_suggestions_source_artifact_id;
