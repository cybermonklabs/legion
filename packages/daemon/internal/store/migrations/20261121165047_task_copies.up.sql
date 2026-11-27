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
-- The outbox check is one list, and main writes it whole above this version: 20261122154643 (the
-- basis seeds) and 20261126110259 (the merge holds change). So this migration writes no list of its
-- own. It adds task_branch to the list the database holds, whichever migration wrote it (the block
-- that ends this file), and ADD CONSTRAINT checks every row the table holds against the new list,
-- as it does for every list. On a database main's release migrated, that list is main's newest, so
-- a pending row of each kind main's daemon queues, pending_basis_seed, reparent_seed and
-- merge_holds_changed among them, is still admitted (store's
-- TestADatabaseMainMigratedWithRowsOfItsNewestKindsUpgrades). On a fresh database it is the design
-- news's list (20261119203047), so each kind main adds above this version is still refused below
-- the migration that adds it (TestTheOutboxTakesTheBasisSeedKindsWith20261122154643). Main's lists
-- above this version leave task_branch out, and 20261126194217, which stands above every one of
-- them, writes the list again with it.
--
-- There is no down file. This directory keeps none, so the way back is the transaction below, run
-- with the daemon stopped, before the earlier image is pinned. It deletes each task_branch row
-- first, since the check written again without the kind refuses a row that holds it, and the
-- earlier release decodes no such row; and each task-join-failed notice not yet sent, a kind the
-- earlier release decodes no row of either. It then takes task_branch out of the list the
-- database holds and keeps every other kind, so a pending row of a kind main added stays admitted:
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   delete from outbox where kind = 'task_branch';
--   delete from outbox where kind = 'notice' and payload->>'kind' = 'task-join-failed';
--   do $$
--   declare
--     kinds text[] := array(select k.m[1] from pg_constraint c,
--       regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''::text', 'g') with ordinality k(m, n)
--       where c.conrelid = 'outbox'::regclass and c.conname = 'outbox_kind_check' order by k.n);
--   begin
--     alter table outbox drop constraint outbox_kind_check;
--     execute format('alter table outbox add constraint outbox_kind_check check (kind in (%s))',
--       (select string_agg(quote_literal(u.kind), ', ' order by u.n)
--         from unnest(array_remove(kinds, 'task_branch')) with ordinality u(kind, n)));
--   end
--   $$;
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

do $$
declare
  kinds text[] := array(select k.m[1] from pg_constraint c,
    regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''::text', 'g') with ordinality k(m, n)
    where c.conrelid = 'outbox'::regclass and c.conname = 'outbox_kind_check' order by k.n);
begin
  if cardinality(kinds) = 0 then
    raise exception 'outbox_kind_check names no kind, so this migration has no list to add task_branch to';
  end if;
  alter table outbox drop constraint outbox_kind_check;
  execute format('alter table outbox add constraint outbox_kind_check check (kind in (%s))',
    (select string_agg(quote_literal(u.kind), ', ' order by u.n)
      from unnest(array_remove(kinds, 'task_branch') || 'task_branch'::text) with ordinality u(kind, n)));
end
$$;
