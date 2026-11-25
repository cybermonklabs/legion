-- 20261123221142_task_base.up.sql — the base a task's start fixed for its pull request.
--
-- plan_tasks.base is the base of the pull request of a task, as its latest start fixed it
-- (record.PlanTask.Base, written by the workflow's start of the task): the base the plan gave the
-- task then. Every reader of a task's base reads it for the task's run (record.StoredPlan.BaseOf),
-- whatever a later version of the plan says of who needs the task or whether it merges. '' is a task
-- that has not started, whose base the plan gives.
--
-- The update writes the base of each task that runs at the upgrade, running or held, at the
-- generation its issue holds, so its run keeps the base it started on. That is the recorded base of
-- the task's open pull request where one is recorded, on the task's branch (the issue's own branch
-- for a task whose row holds none), since it names a branch GitHub holds and the task's last
-- completion was held to it. Else it is the base the release before gave the task, from the same
-- rows: the issue's default branch for a task that no other task of its generation that is not left
-- needs, for a task that needs none, and for a task with no step of the type implementation; its
-- need's branch for a task with one need; and its own branch's join branch for a task with several.
-- A task that waits, is stopped, is done or left keeps ''.
--
-- A daemon of the release before names no plan_tasks.base, so it reads and writes the table as it
-- did with the column standing. The way back drops the column and the version row, so the
-- roll-forward's Migrate applies this file again and writes each running task's base again.
--
-- Nor does that release decode the notice kind pull-request-close-refused, which this release adds:
-- the notice, outbox kind 'notice', that tells the issue's architect the pull requests of tasks
-- whose close GitHub refused as it would refuse it again, so each stays open. It decodes an outbox
-- row strictly and refuses a notice kind it does not know (record.DecodeOutboxPayload, "unknown
-- notice kind"), so such a row left queued fails on every attempt and never finishes, and while its
-- backoff holds it is one of the rows record.Store.WaitingNotices returns, so GET /legion/v1/state
-- answers an error for the whole project, as 20261120042902_claim_agent_resume_generation.up.sql
-- says of the kind it adds. The way back deletes each such notice this release queued and had not
-- sent when the daemon stopped; a sent notice leaves no row. What the delete costs is those
-- tellings: the architect is not told of a close GitHub refused before the daemon stopped.
--
-- There is no down file. This directory keeps none, so the way back is the one transaction that
-- closes this header, its last paragraph (migrations.WayBack), run with the daemon stopped and
-- before the earlier image is pinned, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1.
--
-- begin;
-- delete from outbox where kind = 'notice' and payload->>'kind' = 'pull-request-close-refused';
-- alter table plan_tasks drop column if exists base;
-- delete from schema_version where version = 20261123221142;
-- commit;
alter table plan_tasks add column base text not null default '';
update plan_tasks t set base = b.base
from (
  select t.issue, t.generation, t.task, coalesce(
    (select p.base from pull_requests p
      where p.issue = t.issue and p.state = 'open' and p.base <> ''
        and p.branch = coalesce(nullif(t.branch, ''), 'legion/' || t.issue)),
    case
      when cardinality(t.needs) = 0
        or not exists (select 1 from plan_tasks o where o.issue = t.issue and o.generation = t.generation
          and o.task <> t.task and o.state <> 'left' and t.task = any(o.needs))
        or not exists (select 1 from plan_steps s where s.issue = t.issue and s.generation = t.generation
          and s.task = t.task and s.type = 'implementation')
        then i.default_branch
      when cardinality(t.needs) = 1
        then (select coalesce(nullif(n.branch, ''), 'legion/' || n.issue) from plan_tasks n
          where n.issue = t.issue and n.generation = t.generation and n.task = t.needs[1])
      else coalesce(nullif(t.branch, ''), 'legion/' || t.issue) || '.join'
    end) as base
  from plan_tasks t
  join issues i on i.key = t.issue and i.generation = t.generation
  where t.state in ('running', 'held')
) b
where t.issue = b.issue and t.generation = b.generation and t.task = b.task and coalesce(b.base, '') <> '';
