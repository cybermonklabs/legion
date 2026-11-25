-- Each task, role and agent keeps its own phase entry and handoff. step_task and step_place
-- describe the current generation's attachment; task and agent survive ClearGeneration.
-- A row written before this migration is the only row of its issue and role, so backfilling
-- its existing task and its canonical claim's agent preserves all of its work.
alter table phases add column task text not null default '', add column agent text not null default '';
update phases set task = step_task,
  agent = coalesce(substring(claim from '^legion-[^-]+-[^-]+-[^-]+-([a-z][a-z0-9-]{0,31})$'), role);
alter table phases drop constraint phases_pkey;
alter table phases add primary key (issue, task, role, agent);
