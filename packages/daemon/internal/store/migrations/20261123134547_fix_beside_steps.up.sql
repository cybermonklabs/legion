-- 20261123134547_fix_beside_steps.up.sql — what a fix beside the other steps of a task is recorded
-- in: whether a task's run takes a fix, each fix of a task, each failure of a CI check at a head,
-- the commits and the check of each step's start, the fix a step started beside and the branch it
-- pushes to, the fix, the working copies and the fix's branch of a pending delivery, the fix's
-- working copy of a released workspace's saved work, and the outbox kinds that read a fix's failed
-- job, tell a task's steps of its CI, and delete the branches beside a fix.
--
-- plan_tasks.fixes_ci says the task's run takes a fix when a check that a CI job runs fails at the
-- task's head (record.PlanTask.FixesCI). Every task the upgrade finds belongs to a run that started
-- under the release before, so it reads false, and its issue gets no fix.
--
-- task_fixes holds each fix of a task, numbered from 1 (record.TaskFix): the check and the head it
-- started for, the place of the step it runs again and the entry it raised, its state (reading
-- while the daemon reads the failed job, running once its entry is raised, then ended), and how it
-- ended. pushed holds, for each role, the head of that role's branch beside the fix that a push
-- showed, '{}' for none. A task holds at most one fix that is reading or running
-- (task_fixes_one_open). ci_failures holds each failure of a CI check at a head of a task once
-- (record.CIFailure): whom it went to, the fix that took it and whether the owner was told.
--
-- step_starts.start_commit, end_commit and fix_check are the commits a step's record names at its
-- start and at its end, and the check a fix's record names. step_starts.beside_fix is the open fix
-- the step's start named, 0 for none, and push_branch the branch beside a fix the step pushes to,
-- '' for none; each later start of the place writes both again. pending_task_deliveries.fix is the
-- fix a delivery's task is of, 0 for none, fix_from the head the fix's working copy is made at,
-- move_to the head the task's working copy moves to before the task, and fix_branch the fix's own
-- branch. Each default is what every row the upgrade finds reads: no commit, no check, no fix, no
-- branch.
--
-- workspace_bundles.fix_head is the commit the fix's working copy beside the workspace stood on
-- when a release saved the workspace's work (runtime.WorkspaceBundle.FixHead), '' where none was
-- there, which every saved bundle the upgrade finds reads. The pod that replaces the released one
-- makes the fix's working copy again at that commit.
--
-- The kind fix_job_read reads a fix's failed job from Dispatch before the fix starts
-- (record.FixJobRead), ci_news tells a step of a task a CI report or a change of Dispatch's state
-- (record.CINews), and beside_branches_delete deletes the branches beside a fix on GitHub
-- (record.BesideBranchesDelete). The check is one list, so it is rewritten whole naming every kind,
-- as 20261119203047_design_news rewrote it: that migration's list with the three added. Its version
-- stands above that one on purpose: Migrate applies every unrecorded migration in version order,
-- so the list with the higher version is the last word on any database. issues.hold_reason has no
-- check (0020_hold_reason_and_controller_notice), so the hold reasons fix and ci need no schema.
--
-- There is no down file: migrations here are forward-only. The way back is the one transaction
-- that ends this header, its last paragraph (migrations.WayBack), run with every daemon of the
-- database stopped and before the earlier image is pinned, through
-- psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1, with <PROJECT> the key of the project whose daemon
-- rolls back. The outbox and pending_task_deliveries hold the rows of every project that shares
-- the database, and the tables and columns are the database's, so every daemon of one database
-- moves to this release, and back, together. docs/kubernetes.md carries the same block under
-- "Rolling back the release that starts a fix beside the other steps", and internal/store's
-- TestMigrationFixBesideStepsKeepsEachRowAndItsWayBack runs this one.
--
-- The block runs once for each project: a later run drops nothing again, and deletes and strips
-- that project's rows. The release before decodes a notice, a controller notice, a supervise row,
-- an update ask and a step record strictly, and fails at every attempt a row that carries what it
-- does not know. So the block deletes the queued ci-failed notices and the queued starts of a fix,
-- takes moveTo off each other start, takes branch, ownFix and fixReads off each queued update ask,
-- and takes the two commits and the check off each queued step record and off the record of each
-- step's start, which the roll forward of a rollback below the release that records each step
-- copies into a step record. It takes the fix that owns a merge forward off the pull request's
-- record of it and off each earlier landing it keeps, and leaves every other member and each
-- landing in its order. Three statements take the rows of every project: the release
-- before's kind check, which the block sets back, refuses a fix_job_read, a ci_news and a
-- beside_branches_delete row of any project, and a pending delivery of a fix goes with the columns
-- that name it. That delete reads the column through the row's JSON, so a later run, which finds
-- the column dropped, deletes nothing. An issue held for its fix or for its checks is a plain hold
-- after the block, which a retry of the release before releases. Each deleted notice is one its
-- architect is never told, and each deleted start a fix that does not run; the failed check stands
-- on the pull request, where the release before reads it. A deleted ci_news row is a report or a
-- prompt that nobody gets; a step starts by reading Dispatch, so none is owed after the block. A
-- saved bundle keeps its work when the block drops fix_head: the release before restores it
-- without the fix's working copy, whose commits it still carries. The branches legion-beside/… on
-- GitHub stay: each holds the commits of a step beside a fix, which a person reads before deleting
-- it (docs/kubernetes.md lists them).
--
-- begin;
-- delete from outbox where kind in ('notice', 'controller_notice') and payload->>'kind' = 'ci-failed' and split_part(issue, '-', 1) = '<PROJECT>';
-- delete from outbox where kind = 'fix_job_read';
-- delete from outbox where kind in ('ci_news', 'beside_branches_delete');
-- update outbox set payload = payload - 'branch' - 'ownFix' - 'fixReads' where kind = 'update_ask' and payload ?| array['branch', 'ownFix', 'fixReads'] and split_part(issue, '-', 1) = '<PROJECT>';
-- delete from outbox where kind = 'supervise' and (payload ? 'fix' or payload ? 'fixFrom') and split_part(issue, '-', 1) = '<PROJECT>';
-- update outbox set payload = payload - 'moveTo' where kind = 'supervise' and payload ? 'moveTo' and split_part(issue, '-', 1) = '<PROJECT>';
-- update outbox set payload = payload - 'startCommit' - 'endCommit' - 'fixCheck' where kind = 'step_record' and payload ?| array['startCommit', 'endCommit', 'fixCheck'] and split_part(issue, '-', 1) = '<PROJECT>';
-- update step_starts set record = record - 'startCommit' - 'endCommit' - 'fixCheck' where record ?| array['startCommit', 'endCommit', 'fixCheck'] and split_part(issue, '-', 1) = '<PROJECT>';
-- update pull_requests set merge_forward = (merge_forward - 'fix') || case when jsonb_typeof(merge_forward->'earlier') = 'array' then jsonb_build_object('earlier', (select coalesce(jsonb_agg(landed - 'fix' order by at), '[]'::jsonb) from jsonb_array_elements(merge_forward->'earlier') with ordinality as earlier(landed, at))) else '{}'::jsonb end where (merge_forward ? 'fix' or jsonb_path_exists(merge_forward, '$.earlier[*].fix')) and split_part(issue, '-', 1) = '<PROJECT>';
-- delete from pending_task_deliveries delivery where (to_jsonb(delivery) ->> 'fix')::int > 0;
-- alter table pending_task_deliveries drop column if exists move_to, drop column if exists fix_from, drop column if exists fix, drop column if exists fix_branch;
-- alter table workspace_bundles drop column if exists fix_head;
-- update issues set hold_reason = null where hold_reason in ('fix', 'ci') and split_part(key, '-', 1) = '<PROJECT>';
-- alter table step_starts drop column if exists push_branch, drop column if exists beside_fix, drop column if exists fix_check, drop column if exists end_commit, drop column if exists start_commit;
-- drop table if exists ci_failures;
-- drop table if exists task_fixes;
-- alter table plan_tasks drop column if exists fixes_ci;
-- alter table outbox drop constraint if exists outbox_kind_check;
-- alter table outbox add constraint outbox_kind_check check (kind in ('dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed', 'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake', 'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release', 'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask', 'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record', 'conflict_confirm', 'merge_forward', 'design_changes'));
-- delete from schema_version where version = 20261123134547;
-- commit;
alter table plan_tasks add column fixes_ci boolean not null default false;
create table task_fixes (
  issue text not null, generation bigint not null, task text not null,
  fix int not null check (fix > 0),
  state text not null check (state in ('reading', 'running', 'ended')),
  check_id text not null, head text not null, place int not null check (place > 0),
  entry bigint not null default 0,
  got_report boolean not null default false,
  return_to int not null default 0 check (return_to >= 0),
  failed boolean not null default false,
  end_head text not null default '',
  result text not null default '' check (result in ('', 'passed', 'failed', 'stopped')),
  job_name text not null default '', job_url text not null default '', read_failed text not null default '',
  claim_failed text not null default '',
  way int not null default 0 check (way between 0 and 4),
  pushed jsonb not null default '{}', started_at timestamptz, ended_at timestamptz,
  primary key (issue, generation, task, fix));
create unique index task_fixes_one_open on task_fixes (issue, generation, task) where state in ('reading', 'running');
create table ci_failures (
  issue text not null, generation bigint not null, task text not null,
  check_id text not null, head text not null,
  went_to text not null check (went_to in ('fix', 'implementer', 'merger', 'owner')),
  fix int not null default 0, owner_told boolean not null default false,
  at timestamptz not null,
  primary key (issue, generation, task, check_id, head));
alter table step_starts add column start_commit text not null default '',
  add column end_commit text not null default '', add column fix_check text not null default '',
  add column beside_fix int not null default 0 check (beside_fix >= 0),
  add column push_branch text not null default '';
alter table pending_task_deliveries add column fix int not null default 0,
  add column fix_from text not null default '', add column move_to text not null default '';
alter table pending_task_deliveries add column fix_branch text not null default '';
alter table workspace_bundles add column fix_head text not null default '';

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'fix_job_read', 'ci_news',
  'beside_branches_delete'));
