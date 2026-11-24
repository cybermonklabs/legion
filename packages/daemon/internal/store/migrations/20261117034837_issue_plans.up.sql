-- 20261117034837_issue_plans.up.sql — the plan of an issue as rows, and the step a phases row runs
-- (record's plan.go).
--
-- issue_plans holds each version given for an issue at a generation: the owner's version of its
-- plan document (source owner, given_by the claim token of the owner that gave it), or the
-- project's default steps (source default, version 0, no document and no giver). The highest
-- version of a generation is the plan in force. plan_tasks holds each task of the plan, what its
-- block says (steps_by_default: the block names no step, so the task's steps are the default steps
-- it runs, and a later version is compared with a block that names none), where it stands (state,
-- running_kind, position) and its counts of returns, which an issue with no plan rows keeps on its
-- implementer's phases row. plan_steps holds each step of a task by its place from 1. The count of
-- starts of a step is not here: step_starts keeps it by place alone (20261117004117_step_starts).
-- No table names an issues row, because an owner can give a plan before the daemon records its
-- issue, which is written at generation 0.
--
-- phases.step_task and phases.step_place are record.PhaseRow.StepTask and StepPlace, the step a
-- role's row runs for an issue with plan rows. Every row the upgrade finds is the row of an issue
-- with no plan rows, since no release before this one ran a plan, and takes the defaults, which
-- read as no step.
--
-- There is no down file. This directory keeps none, so the way back is the transaction that ends
-- this header, its last paragraph (migrations.WayBack), run with every project's daemon on that
-- database stopped, since it drops tables each WriteIssue of this release reads, before the
-- earlier image is pinned, with <PROJECT> the key of the project whose daemon rolls back.
-- docs/kubernetes.md carries the same block under "Rolling back the release whose owner's plan
-- picks the steps": internal/textcheck's TestTheOwnerStepsWayBackIsOneTextInTheHeaderAndTheGuide
-- holds the two to one text, and internal/store's owner_steps_way_back_test.go runs this one.
--
-- The release before this one boots without the drops: its phases reads and writes name each
-- column they touch, and it names none of the three tables. They are owed to the roll forward.
-- Left in place, a task's place goes stale while the earlier release moves its issue along today's
-- fixed line, and after the roll forward the walk would start from that stale place.
-- With the tables and columns dropped and the row deleted, store.apply runs this file again, and
-- each issue the earlier release moved has no plan rows and walks today's line. What the way back
-- costs is every plan given before the rollback, which its owner gives again.
--
-- The deletes and the update are owed to the release before: it decodes a notice, a controller
-- notice and a supervise row strictly, knows no notice of the kinds task-stopped, retro-held and
-- plan-not-taken, nor the stoppedBy a stop's notice carries, and no op step_stop, and it fails such
-- a row at every attempt. Each deleted notice is one its architect, or the controller, is never
-- told, and each deleted step_stop a step's process that runs on until its issue closes. It serves
-- issues.hold_reason as the record holds it, and its plugin reads holdReason with the one value
-- escalated and refuses the whole state for another, so the update clears the reasons this release
-- writes, task-stopped and children: each issue held for either is a plain hold after it, which a
-- retry of either release releases, the stopped step started again and a parent's next phase
-- started while the issues beneath it may still run (docs/kubernetes.md says what to do of each).
-- The outbox and the issue records are shared by every project's daemon on the database, so each
-- delete and the update take the rows of the project that rolls back alone. The tables and the
-- columns are the database's, so every project's daemon on it rolls back too, and the block runs
-- once for each project: a later run drops nothing again and deletes that project's rows.
--
-- begin;
-- drop table if exists plan_steps;
-- drop table if exists plan_tasks;
-- drop table if exists issue_plans;
-- alter table phases drop column if exists step_task, drop column if exists step_place;
-- delete from outbox where kind in ('notice', 'controller_notice') and payload->>'kind' in ('task-stopped', 'retro-held', 'plan-not-taken') and split_part(issue, '-', 1) = '<PROJECT>';
-- delete from outbox where kind = 'supervise' and payload->>'op' = 'step_stop' and split_part(issue, '-', 1) = '<PROJECT>';
-- update issues set hold_reason = null where hold_reason in ('task-stopped', 'children') and split_part(key, '-', 1) = '<PROJECT>';
-- delete from schema_version where version = 20261117034837;
-- commit;
create table issue_plans (
  issue text not null,
  generation bigint not null,
  version integer not null,
  source text not null constraint issue_plans_source_check check (source in ('owner', 'default')),
  artifact_id text not null,
  given_by text not null,
  given_at timestamptz not null,
  primary key (issue, generation, version)
);

create table plan_tasks (
  issue text not null,
  generation bigint not null,
  task text not null,
  plan_version integer not null,
  block_text text not null,
  needs text[] not null default '{}',
  part_of text not null default '',
  owner_role text not null default '',
  merges boolean not null default false,
  steps_by_default boolean not null default false,
  state text not null constraint plan_tasks_state_check check (state in ('waiting', 'running', 'done', 'stopped', 'left')),
  stopped_reason text not null default '',
  running_kind text not null default '' constraint plan_tasks_running_kind_check check (running_kind in ('', 'step', 'retro', 'merge')),
  position integer not null default 0 constraint plan_tasks_position_check check (position >= 0),
  pass integer not null default 0,
  rounds integer not null default 0,
  conflict_rounds integer not null default 0,
  backward_moves jsonb not null default '{}',
  cap_told integer not null default 0,
  primary key (issue, generation, task),
  -- A step stands at a place from 1; a retro, a merge and no act at place 0.
  constraint plan_tasks_place_check check ((running_kind = 'step') = (position >= 1)),
  foreign key (issue, generation, plan_version) references issue_plans (issue, generation, version) on delete cascade
);

create table plan_steps (
  issue text not null,
  generation bigint not null,
  task text not null,
  position integer not null constraint plan_steps_position_check check (position >= 1),
  role text not null constraint plan_steps_role_check check (role ~ '^[a-z][a-z0-9-]*$'),
  type text not null,
  agent_key text not null default '',
  primary key (issue, generation, task, position),
  foreign key (issue, generation, task) references plan_tasks (issue, generation, task) on delete cascade
);

alter table phases add column step_task text not null default '', add column step_place integer not null default 0;
