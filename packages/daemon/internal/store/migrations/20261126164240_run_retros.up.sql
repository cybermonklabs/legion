-- 20261126164240_run_retros.up.sql — the record of each run's retro, from which Legion decides
-- whether an issue that closes owes the retro it runs after the close, and the mark a claim and
-- its delivery carry of the retro after a close their worker took (INFRA-5602).
--
-- run_retros is record.RunRetro: one row for each issue and run (the issue's generation).
-- started_at is when the run started, at its own first phase or at the first phase of an issue
-- beneath it; retro_started_at is when a retro step of the run first started, whatever its result,
-- and null while none has. The rest is the retro Legion runs after the issue's close, which the
-- close opens on the newest run that started where that run had no retro:
-- opened_at; bound_at, when it ends if nobody reported it (workflow.RetroAfterCloseBound after its
-- retro's start); agent, the claim token of the agent that does it; entry, the phase entry of a
-- retro already running at the close and 0 otherwise; and ended_at, outcome and ended_by once it
-- ends (record.RetroOutcome, record.RetroEndCause). The checks hold those together: a bound, an
-- agent and an entry only on an opened retro, which has its retro's start, and an end with its
-- outcome and its cause only on an opened retro. run_retros_open_tree is the open retros after a
-- close of a tree by bound (record.Postgres.OpenRetrosOfTree), which the linger, the pod gate and
-- the root architect's wake read.
--
-- The fill writes a row for the current run of each recorded issue that started, from what the
-- database holds, and, for an issue whose current run did not start, a row for its newest run that
-- did, since a close reads the newest run of any generation (the workflow's retroAtClose) and owes
-- that run's retro where it had none: the highest earlier generation one of the issue's step starts
-- names, started at its earliest step start, its retro had where a retro or a merge step start of
-- it stands. No other older run gets a row, and no issue above is marked started from one. A run
-- started where its issue stands in a phase that carries a run (not admitted, awaiting_person or
-- done, and not held from admitted, awaiting_person or done) or holds a step start of the run's
-- generation (step_starts, whose record names the generation of each place's latest start). Each
-- issue above such an issue in its tree started its own current run too, where that start came after every
-- step start the issue above holds of an earlier run of its own: a parent held at admitted for its
-- children (admit's waitsForChildren) above a child that started is started, and a root
-- re-admitted after its tree finished is not started by the children of its earlier run, which
-- started before that run's own phases. issues.taken_back is not read: it survives a root's
-- re-admission, so it says nothing of the current run. started_at is the run's earliest step
-- start, else the issue's last movement (issues.moved_at), and above an issue the earliest of
-- those beneath it and its own.
-- A run had its retro where a retro or a merge step start of its generation stands, whatever its
-- result, or its issue stands in retro or merging, or is held from merging, or from retro for a
-- reason other than its children (a hold for its children stands in place of the retro), or the
-- merger's phases row holds a step (step_task, which a re-admission clears; it keeps the entry, so
-- the entry says nothing of the current run). retro_started_at is the earliest such step start,
-- else the merger's entry, else the issue's last movement. No row opens a retro after a close:
-- no release before this one ran one.
--
-- pending_task_deliveries.retro_run and claims.serving_retro_run are the run of the retro after a
-- close a delivery is of and a claim was left serving (supervise.Delivery.RetroRun and
-- supervise.Claim.ServingRetroRun), the retro side of check_look and serving_check_look
-- (20261105231916_production_check_role_and_wake.up.sql): what says a completion is that retro's
-- report rather than a phase completion. 0 is a delivery, or a claim, of no such retro, which is
-- every row the upgrade finds.
--
-- The outbox's kind check is one list, so it is rewritten whole naming every kind: the list of
-- 20261126163320_state_events.up.sql, its thirty-six kinds in its order, with retro_bound added,
-- the row that ends a retro after a close nobody reported at its bound (record.RetroBound),
-- thirty-seven in all. Its version stands above that list's, and so above the lists of
-- 20261126110259, 20261122154643, 20261119203047, 20261119003157, 20261117023932 and
-- 20261117000528, which that list stands above too, so on any database this list is the last
-- word: Migrate applies every unrecorded migration in version order, and at a lower version any of
-- those lists, none of which names retro_bound, would be applied after this one and refuse the
-- kind. This list must name the three state kinds (state_change, state_admission, state_boot),
-- merge_holds_changed and every other kind of that list, because a database main's release
-- migrated can hold a pending row of each of those kinds when this migration is applied over it,
-- and a list without the kind would fail its ADD CONSTRAINT on that row (SQLSTATE 23514), and
-- Migrate with it. A list without the state kinds would, besides, be the last word on every
-- database and refuse every state row the daemon queues.
--
-- This migration was numbered 20261125213821 on earlier builds of this branch until the forward
-- merge of 2026-11-26 brought in 20261126110259, which stood above it and left retro_bound out. No
-- deployment ran that version. It stands below a list that does not name retro_bound, so on a
-- fresh database that list would have been the last word and refused the kind, and on a database
-- main's release migrated it was applied after 20261126110259, where its list, without
-- merge_holds_changed, would have refused a pending row of that kind. A database a build of this
-- branch migrated before this merge records the older version, and it is a test's own, to make
-- again.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon of the database stopped and before the
-- release before starts, through PGPASSFILE=<pgpass file> psql '<postgres_url>' -X -v
-- ON_ERROR_STOP=1 (docs/kubernetes.md, "Reaching the daemon's database by hand"). The three deletes of
-- queued rows are owed to the release before: it decodes an outbox payload strictly
-- (record.DecodeOutboxPayload, DisallowUnknownFields) and knows neither the kind retro_bound, nor a
-- supervise row's retroRun or retroWake, nor a linger_close row's retrosEnded, so it would fail
-- each such row at every attempt, and its issue's lifecycle lane behind it. Each deleted row is a
-- bound or a retro task or a root architect's wake that a release which runs no retro after a close
-- owes nobody, or the close a tree past its linger owes the hour after its last retro after a close
-- ended. That release names neither the table nor the two columns in any read or write, so it boots
-- with them standing: the drops and the delete of this version's row are owed to the roll-forward.
-- With them gone, store.apply finds no row of this version and runs this file again, which fills
-- run_retros from the issues as they stand then. What the way back costs is each retro after a
-- close open at the rollback: the roll-forward opens none again, and its agent's next completion is
-- refused as one on a done issue. It costs as well the close of what a tree's linger left to its
-- retros after a close: a tree whose linger ended while one was open, or whose last one ended within
-- the hour before the rollback, keeps the claims and the workspace of each such retro's issue, and
-- its Sandbox, until a todo on its root re-admits the tree and its next linger ends. The kind check
-- keeps retro_bound beside every kind of 20261126163320's list, the three state kinds among them,
-- so it admits every kind the release before writes. The outbox, claims and pending_task_deliveries
-- hold the rows of every project that shares the database, so every daemon of one database moves
-- back, and forward, together.
--
-- begin;
-- delete from outbox where kind = 'retro_bound';
-- delete from outbox where kind = 'supervise' and (payload ? 'retroRun' or payload ? 'retroWake');
-- delete from outbox where kind = 'linger_close' and payload ? 'retrosEnded';
-- alter table claims drop column if exists serving_retro_run;
-- alter table pending_task_deliveries drop column if exists retro_run;
-- drop table if exists run_retros;
-- delete from schema_version where version = 20261126164240;
-- commit;
create table run_retros (
  issue text not null,
  generation bigint not null check (generation >= 1),
  tree text not null,
  started_at timestamptz not null,
  retro_started_at timestamptz,
  opened_at timestamptz,
  bound_at timestamptz,
  agent text not null default '',
  entry bigint not null default 0 check (entry >= 0),
  ended_at timestamptz,
  outcome text not null default '' check (outcome in ('', 'passed', 'stopped')),
  ended_by text not null default '' check (ended_by in ('', 'report', 'bound', 'todo')),
  primary key (issue, generation),
  check ((opened_at is null) = (bound_at is null)),
  check ((opened_at is null) = (agent = '')),
  check (opened_at is null or retro_started_at is not null),
  check (entry = 0 or opened_at is not null),
  check ((ended_at is null) = (outcome = '')),
  check ((ended_at is null) = (ended_by = '')),
  check (ended_at is null or opened_at is not null)
);

create index run_retros_open_tree on run_retros (tree, bound_at, issue)
  where opened_at is not null and ended_at is null;

alter table pending_task_deliveries add column retro_run bigint not null default 0 check (retro_run >= 0);
alter table claims add column serving_retro_run bigint not null default 0 check (serving_retro_run >= 0);

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'state_change', 'state_admission', 'state_boot', 'retro_bound'));

-- own is each recorded issue's current run that started of itself: the issue stands in a phase
-- that carries a run, or holds a step start of the run's generation. started_at is the run's
-- earliest step start, else the issue's last movement.
with recursive own as (
  select i.key as issue, i.generation, i.tree, i.parent,
    coalesce(
      (select min(s.started_at) from step_starts s
        where s.issue = i.key and coalesce((s.record->>'generation')::bigint, 0) = i.generation),
      i.moved_at) as started_at
  from issues i
  where i.generation >= 1
    and (coalesce(case when i.phase = 'held' then i.held_from end, i.phase) not in ('admitted', 'awaiting_person', 'done')
      or exists (select 1 from step_starts s
        where s.issue = i.key and coalesce((s.record->>'generation')::bigint, 0) = i.generation))
),
-- above carries each such start up the issue's tree, to each issue above it at that issue's own
-- current run, for as long as the start came after every step start the issue above holds of an
-- earlier run of its own; path stops a cycle of parents.
above (issue, generation, tree, parent, started_at, path) as (
  select o.issue, o.generation, o.tree, o.parent, o.started_at, array[o.issue] from own o
  union all
  select p.key, p.generation, p.tree, p.parent, a.started_at, a.path || p.key
  from above a
  join issues p on p.key = a.parent and p.tree = a.tree
  where p.generation >= 1
    and p.key <> all(a.path)
    and a.started_at > coalesce((select max(s.started_at) from step_starts s
      where s.issue = p.key and coalesce((s.record->>'generation')::bigint, 0) <> p.generation), '-infinity')
),
started as (
  select issue, generation, tree, min(started_at) as started_at from above group by issue, generation, tree
),
-- retro is each recorded issue's current run that had its retro, and when that retro started.
retro as (
  select i.key as issue, i.generation,
    coalesce(
      (select min(s.started_at) from step_starts s
        where s.issue = i.key and s.kind in ('retro', 'merge') and coalesce((s.record->>'generation')::bigint, 0) = i.generation),
      (select min(p.entry_raised_at) from phases p where p.issue = i.key and p.role = 'merger' and p.step_task <> ''),
      i.moved_at) as retro_started_at
  from issues i
  where i.generation >= 1
    and (i.phase in ('retro', 'merging')
      or (i.phase = 'held' and i.held_from = 'merging')
      or (i.phase = 'held' and i.held_from = 'retro' and coalesce(i.hold_reason, '') <> 'children')
      or exists (select 1 from step_starts s
        where s.issue = i.key and s.kind in ('retro', 'merge') and coalesce((s.record->>'generation')::bigint, 0) = i.generation)
      or exists (select 1 from phases p where p.issue = i.key and p.role = 'merger' and p.step_task <> ''))
),
-- older is, for each recorded issue whose current run did not start, its newest run that did: the
-- highest earlier generation one of its step starts names, which a close owes the retro of where
-- it had none (the workflow's retroAtClose reads the newest run of any generation). It started at
-- its earliest step start and had its retro where a retro or a merge step start of it stands.
older as (
  select i.key as issue, n.generation, i.tree,
    (select min(s.started_at) from step_starts s
      where s.issue = i.key and coalesce((s.record->>'generation')::bigint, 0) = n.generation) as started_at,
    (select min(s.started_at) from step_starts s
      where s.issue = i.key and s.kind in ('retro', 'merge') and coalesce((s.record->>'generation')::bigint, 0) = n.generation) as retro_started_at
  from issues i
  cross join lateral (select max(coalesce((s.record->>'generation')::bigint, 0)) as generation from step_starts s
    where s.issue = i.key and coalesce((s.record->>'generation')::bigint, 0) between 1 and i.generation - 1) n
  where n.generation is not null and not exists (select 1 from started st where st.issue = i.key)
)
insert into run_retros (issue, generation, tree, started_at, retro_started_at)
select s.issue, s.generation, s.tree, s.started_at, r.retro_started_at
from started s left join retro r on r.issue = s.issue and r.generation = s.generation
union all
select o.issue, o.generation, o.tree, o.started_at, o.retro_started_at from older o;
