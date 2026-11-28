-- 20261127224354_observed_step_role.up.sql — the role of the running step that Dispatch last
-- observed of an issue, for the state event that moves it.
--
-- An issue at step, or held from it, is worked by the role of its task's running step, a role the
-- deployment defines, which the state route serves as the issue's `stepRole` (record.Issue.StepRole,
-- read from the plan rows of the issue's generation). Dispatch observes it as it observes the
-- phase, and puts a step's hours on that role. A state event names each field of an issue that a
-- write moved (record's PutIssue), comparing the row it replaces with the row it writes. The role
-- lives in the plan rows, not on the issues row, and the workflow moves the task before it writes
-- the issue (its moveTask, then WriteIssue), so read from the plan rows the role before the write
-- is already the role after it: a move from one step of the deployment to another, with the issue
-- at step on both sides, would name nothing. issues.observed_step_role keeps the role as the issue's
-- last write left it, empty where the issue is neither at step nor held from it, so the next write
-- compares against it. PutIssue writes it from the plan rows on every write.
--
-- Every row takes the default, empty, but an issue at step or held from step whose plan rows of
-- its generation name a running step, which takes that step's role: the value its next write would
-- write, so that write names no change the upgrade made up.
--
-- The release before this one names each column of the table it writes (record's PutIssue) and
-- reads none of this one, so every row it inserts takes the default. A daemon of this release then
-- writing such an issue at step names its step role from null, which the issue held all along: the
-- value Dispatch's next whole read of the state finds too.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon of the database stopped and before the
-- release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. issues holds the rows
-- of every project that shares the database, and a daemon of this release reads the column in every
-- write of a record (record's observedIssueColumns), so one still running for another project after
-- the drop would fail every write of an issue.
--
-- The drop and the delete of this migration's row go together. The release before this one needs
-- neither to run, since it never names the column: the delete is what makes the version the store
-- reports, its highest recorded row (store.SchemaVersionTx), that release's own again. Both are
-- owed to the roll-forward. With the column dropped and the row deleted, store.apply finds no row
-- of this version and runs this file again, which adds the column again and fills it as the
-- paragraph above says; an add of a column that stands would fail.
--
-- begin;
-- alter table issues drop column if exists observed_step_role;
-- delete from schema_version where version = 20261127224354;
-- commit;
alter table issues add column observed_step_role text not null default '';

update issues set observed_step_role = coalesce((
  select s.role from plan_tasks t
    join plan_steps s on s.issue = t.issue and s.generation = t.generation and s.task = t.task and s.position = t.position
   where t.issue = issues.key and t.generation = issues.generation and t.running_kind = 'step'), '')
 where phase = 'step' or (phase = 'held' and held_from = 'step');
