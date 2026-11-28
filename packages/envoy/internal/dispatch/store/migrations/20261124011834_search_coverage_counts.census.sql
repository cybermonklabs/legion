-- 20261124011834 creates four tables, which hold no row at this schema, two indexes on one of them,
-- the functions the triggers run, and triggers on embeddings, components, artifact_versions,
-- check_results, handoffs, issues and artifacts, which run only on later writes: no existing row is
-- read, refused or rewritten. It cannot name the new tables, which do not exist when the census is
-- taken.
select 0;
