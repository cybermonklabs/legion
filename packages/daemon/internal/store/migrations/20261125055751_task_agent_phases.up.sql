-- The earlier daemon stores one phase row per issue and role. With both daemons stopped, the
-- way back below refuses rows it cannot preserve or reconstruct; it never picks one task's
-- handoff over another. Finish the affected work and archive its phase rows before removing
-- any row named by a refusal. Reverse later migrations first, then run:
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   lock table phases in access exclusive mode;
--   do $$
--   declare found_key text;
--   begin
--     select issue || '/' || role into found_key from phases group by issue, role having count(*) > 1 order by issue, role limit 1;
--     if found_key is not null then
--       raise exception 'Cannot restore one phase row per role: % has more than one task agent', found_key;
--     end if;
--     select issue || '/' || role into found_key from phases
--       where task <> step_task or agent <> coalesce(substring(claim from '^legion-[^-]+-[^-]+-[^-]+-([a-z][a-z0-9-]{0,31})$'), role)
--       order by issue, role limit 1;
--     if found_key is not null then
--       raise exception 'Cannot reconstruct the task and agent of phase % after rolling forward', found_key;
--     end if;
--   end $$;
--   alter table phases drop constraint phases_pkey;
--   alter table phases drop column task, drop column agent;
--   alter table phases add primary key (issue, role);
--   delete from schema_version where version = 20261125055751;
--   commit;
--   SQL
--
-- Each task, role and agent keeps its own phase entry and handoff. step_task and step_place
-- describe the current generation's attachment; task and agent survive ClearGeneration.
-- A row written before this migration is the only row of its issue and role, so backfilling
-- its existing task and its canonical claim's agent preserves all of its work.
alter table phases add column task text not null default '', add column agent text not null default '';
update phases set task = step_task,
  agent = coalesce(substring(claim from '^legion-[^-]+-[^-]+-[^-]+-([a-z][a-z0-9-]{0,31})$'), role);
alter table phases drop constraint phases_pkey;
alter table phases add primary key (issue, task, role, agent);
