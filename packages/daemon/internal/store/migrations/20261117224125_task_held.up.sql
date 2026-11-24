-- 20261117224125_task_held.up.sql — a task held where it stands, for the reason of its hold, and
-- what the step at a running task's place waits for.
--
-- plan_tasks.state takes held (record.TaskHeld): the task stands where it stood, and nothing moves
-- it until its owner's retry releases it at the same place (record's HoldTask and ReleaseTask).
-- plan_tasks.hold_reason says why (record.PlanTask.HoldReason): '' the claim of the agent at its
-- running step failed and its owner has not decided, escalated its owner escalated that hold, and
-- children the task that merges waits before its retro for the issues beneath its issue. Only a
-- held task holds a reason (plan_tasks_hold_reason_held_check), so the end of a hold ends its
-- reason, as 0020's trigger ends an issue's.
--
-- plan_tasks.step_wait and plan_tasks.step_wait_since are what the step at a running task's place
-- waits for before its start is delivered, and since when (record's SetStepWait): '' nothing, join
-- the end of its task's join, agent its agent, which runs a step of another task. A wait has its
-- moment and no wait has none (plan_tasks_step_wait_since_check). A hold takes only a step that was
-- delivered, whose wait is '': the failed claim is of a step its agent was given, and an
-- escalation and the hold before a retro hold a step that has no wait either. So a held step
-- never has a wait.
--
-- Each task the upgrade finds keeps its row, with no hold reason and no step wait, but one: the
-- running task of each issue held at the upgrade is written held with the issue's hold reason (an
-- issue's NULL reason is '', the hold of a failed claim), so the hold is one fact of the task and no
-- reader takes it from the issue. An issue held because its owner stopped the task it ran
-- (task-stopped) has that task stopped already, and the update leaves it so. It changes no outbox
-- kind.
--
-- There is no down file. This directory keeps none, so the way back is the transaction below, run
-- with the daemon stopped, before the earlier image is pinned. It first writes each held task
-- running again, with no hold reason, which only a held task may hold: the earlier release knows no
-- such state, and the state check written again without it refuses a row that holds it:
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   update plan_tasks set state = 'running', hold_reason = '' where state = 'held';
--   alter table plan_tasks drop column if exists step_wait_since, drop column if exists step_wait,
--     drop column if exists hold_reason;
--   alter table plan_tasks drop constraint if exists plan_tasks_state_check;
--   alter table plan_tasks add constraint plan_tasks_state_check
--     check (state in ('waiting', 'running', 'done', 'stopped', 'left'));
--   delete from schema_version where version = 20261117224125;
--   commit;
--   SQL
--
-- The release before this one reads and writes plan_tasks by the columns it names, and it names
-- none of the three, so their drops are owed to the roll forward: with the columns dropped and the
-- row deleted, store.apply runs this file again, which adds the columns again at their defaults and
-- holds the running task of each issue held then. What the way back costs is each hold of a task
-- whose issue was not held, which no release before this one writes: that task reads running at
-- its place. A task held with its issue reads running beside the issue's own hold, which the
-- earlier release reads as it always did. And each step's wait: the step reads as one whose start
-- is due.
alter table plan_tasks drop constraint if exists plan_tasks_state_check;
alter table plan_tasks add constraint plan_tasks_state_check
  check (state in ('waiting', 'running', 'held', 'done', 'stopped', 'left'));

alter table plan_tasks
  add column hold_reason text not null default ''
    constraint plan_tasks_hold_reason_check check (hold_reason in ('', 'escalated', 'children')),
  add column step_wait text not null default ''
    constraint plan_tasks_step_wait_check check (step_wait in ('', 'join', 'agent')),
  add column step_wait_since timestamptz;

alter table plan_tasks
  add constraint plan_tasks_hold_reason_held_check check (state = 'held' or hold_reason = ''),
  add constraint plan_tasks_step_wait_since_check check ((step_wait = '') = (step_wait_since is null));

update plan_tasks t set state = 'held', hold_reason = coalesce(i.hold_reason, '')
  from issues i
  where i.key = t.issue and i.generation = t.generation and i.phase = 'held' and t.state = 'running'
    and coalesce(i.hold_reason, '') in ('', 'escalated', 'children');
