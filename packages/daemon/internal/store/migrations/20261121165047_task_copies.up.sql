-- 20261121165047_task_copies.up.sql — the branch of each task of a plan, the needs whose join
-- GitHub refused, the names a generation gave to its tasks, and the outbox kind that makes a task's
-- branch on GitHub (task_branch).
--
-- plan_tasks.branch is the GitHub branch of the task (record.PlanTask.Branch), written once, when
-- the task first starts (record's AssignTaskBranch): legion/<KEY> for the first task of the
-- issue to start, legion/<KEY>.<slug> for each other. '' reads as legion/<KEY>
-- (record.PlanTask.BranchOf), which is the branch of every task the upgrade finds, since no release
-- before this one gave a task a branch of its own. plan_tasks.join_conflicts is the id of each need
-- of the task whose merge GitHub refused as a conflict, into the branch the join is made on: the
-- task's join branch where it has one (record.StoredPlan.BaseOf), else its branch, which the task's
-- first step joins by hand there (record's SetJoinConflicts). plan_tasks.join_unmerged says GitHub
-- refused as a conflict the merge of the task's join branch into the task's branch, which the
-- task's own work there makes at a start again; the first step merges it by hand on the task's
-- branch (record's SetJoinUnmerged). Each join writes both again. plan_tasks.start_reason is why
-- the step the task stands at was started (record's SetStartReason): a start again at its first
-- step, or a move back to an earlier step; '' for a first start. A start again of a stopped task
-- waits for the join of its needs, and a moved-back step can wait for its agent, and the step that
-- starts after such a wait tells its agent why and counts as a move back, as a start that waits
-- for nothing does. Every move of the task clears it.
--
-- plan_branches is each branch name a generation of an issue gave to one of its tasks. GitHub keeps
-- a branch and its commits after the task that held it was stopped and a later version removed it,
-- and after the generation that gave it ended, so a name given is never given to another task of
-- the issue, and the deleted task's row cannot say it was given. AssignTaskBranch reads the table,
-- and so does the workflow where it records a pull request on the issue's own branch
-- (BranchGiven); it names no task, so a task's branch has one place, plan_tasks.branch.
--
-- The outbox check is one list, and the last migration to write it owns every value of it, main's
-- included: Migrate applies every unrecorded migration in version order, so on a database main
-- migrated this file runs after main's, late, and its list is the last word there. So it names
-- every kind: 20261119203047's thirty, task_branch, and main's two basis seeds
-- (pending_basis_seed and reparent_seed, main's 20261122154643, which sorts above this file).
-- Without them an upgrade of a database main migrated stops on a queued seed row (SQLSTATE
-- 23514) and the daemon does not boot. A fresh database is unharmed: the two kinds arrive here
-- rather than at main's migration, and 20261126144204 writes the list again above both.
--
-- There is no down file. This directory keeps none, so the way back is the transaction below, run
-- with the daemon stopped, before the earlier image is pinned. It deletes each task_branch row
-- first, since the check written again without the kind refuses a row that holds it, and the
-- earlier release decodes no such row; and each task-join-failed notice not yet sent, a kind the
-- earlier release decodes no row of either. The check it writes again keeps main's two basis seeds
-- (pending_basis_seed and reparent_seed), which this file's own list does not name: main's
-- 20261122154643 stays recorded after the way back, and a queued row of either stays with it:
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   delete from outbox where kind = 'task_branch';
--   delete from outbox where kind = 'notice' and payload->>'kind' = 'task-join-failed';
--   alter table outbox drop constraint if exists outbox_kind_check;
--   alter table outbox add constraint outbox_kind_check check (kind in (
--     'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
--     'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
--     'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
--     'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
--     'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
--     'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed'));
--   alter table plan_tasks drop column if exists branch, drop column if exists join_conflicts,
--     drop column if exists join_unmerged, drop column if exists start_reason;
--   drop table if exists plan_branches;
--   delete from schema_version where version = 20261121165047;
--   commit;
--   SQL
--
-- The release before this one boots without the statements: its plan_tasks reads and writes name
-- each column they touch, and it names none of the four new columns nor the new table. They are
-- owed to the roll forward. With the columns and the table dropped and the row deleted, store.apply
-- runs this file again, and each task the roll forward finds reads the issue's own branch. What the
-- way back costs is the branch of each task that started on a branch of its own: the roll forward
-- reads the issue's branch for it, so its next step works on the issue's branch, not on the task's;
-- and the reason of each start again whose step still waits for its join, whose step then starts
-- with no reason and not as a move back.
--
-- The way back drops plan_branches with the four columns (`drop table if exists plan_branches`
-- above): the earlier release names it in no statement, and the roll forward creates it again
-- empty. What that costs is the names the generations gave: after the roll forward, a task that
-- starts takes a name again by the rows alone, so it can take the name of a task that a later
-- version removed before the rollback, and that branch's commits.
alter table plan_tasks add column branch text not null default '',
  add column join_conflicts text[] not null default '{}',
  add column join_unmerged boolean not null default false,
  add column start_reason text not null default '';

create table plan_branches (
  issue text not null,
  generation bigint not null,
  branch text not null,
  primary key (issue, generation, branch)
);

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'task_branch', 'pending_basis_seed',
  'reparent_seed', 'merge_holds_changed'));
