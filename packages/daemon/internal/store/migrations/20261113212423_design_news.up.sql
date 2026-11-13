-- 20261113212423_design_news.up.sql — what each holder of a tree's work took of its approved
-- design, the change sets between two approved versions, the news a delivery carries, and the
-- outbox kind that reconciles the two (dispatch://INFRA-175).
--
-- Until this migration nothing recorded what anyone was told of an approved version. The opening
-- message was composed when the gate flipped and delivered later to whoever held the role then,
-- and no row said which version that holder's work stood on. News of a new version is owed where
-- the gate's approved version differs from the version the holder took, so the record now states
-- what each holder took, and a reconcile closes the difference (record.OwedNews).
--
-- design_news_told holds one row per issue and audience. The `holder` row of an issue is the
-- approved version of each gated document that a session holding that issue's phase took. The
-- `architect` row of a tree root is what that tree's root architect took for the tree. versions
-- is a JSON object of step to version: a step it does not name is a version the record does not
-- know, and 0 is a step taken while no approved version of its document stood. opening_owed says
-- the opening sentence of the gate is owed to that audience: the gate opened while the holder
-- was owed news, so the news task carries the opening in place of the role message. told_at is
-- when the row last rose, null for a row that carries only the owed opening. A row goes with its
-- issue's generation (record.Postgres.clearGeneration), beside the issue's gate rows.
--
-- design_changes holds what changed in one document from one version to another, read from
-- Dispatch (GET /api/v1/artifacts/{id}/changes). Two stored versions of a document never change,
-- so a recorded change set never goes stale, and keeping it is no copy of a value its owner can
-- move. state `read` is Dispatch's answer: decision_only, and the sections and typed blocks that
-- differ. state `unavailable` is a pair Dispatch could not answer, a release without the route or
-- a bounded run of failures, with the reason in reason; the news then names the two versions and
-- says why it names no change. from_version 0 is a pair whose document had no earlier approved
-- version.
--
-- pending_task_deliveries.design_news is the news items a delivery carries, each the issue, the
-- audience, the step, the document and the version pair, and whether it carries the opening: what
-- the confirmation of that delivery raises in design_news_told. It is null for a delivery that
-- carries no news, which is every delivery the release before this one queued.
--
-- The outbox kind design_changes is the row that runs that reconcile for one tree. The check is
-- one list, so it is rewritten whole naming every kind, as 20261110235320 did.
--
-- Rolling the image back needs no restore of the schema. The release before this one names
-- neither table nor the new column in any statement, its pending_task_deliveries insert names its
-- own columns and leaves design_news null, and the kind list it set is a subset of this one, so
-- every row it writes is still taken. A design_changes row this release queued sits in the outbox
-- unread under that daemon, which runs no lane of that kind, and runs once this release is back.
--
-- There is no down file: migrations here are forward-only.
create table design_news_told (
  issue text not null references issues (key) on delete cascade,
  audience text not null check (audience in ('holder', 'architect')),
  versions jsonb not null default '{}'::jsonb check (jsonb_typeof(versions) = 'object'),
  opening_owed boolean not null default false,
  told_at timestamptz,
  primary key (issue, audience));

create table design_changes (
  artifact_id text not null,
  from_version integer not null check (from_version >= 0),
  to_version integer not null,
  state text not null check (state in ('read', 'unavailable')),
  decision_only boolean not null default false,
  sections jsonb not null default '[]'::jsonb check (jsonb_typeof(sections) = 'array'),
  blocks jsonb not null default '[]'::jsonb check (jsonb_typeof(blocks) = 'array'),
  reason text not null default '',
  read_at timestamptz not null,
  primary key (artifact_id, from_version, to_version),
  check (to_version > from_version),
  -- A pair Dispatch could not answer says why, and a pair it answered has no reason to give.
  check ((state = 'unavailable') = (reason <> '')));

alter table pending_task_deliveries add column design_news jsonb;

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'design_changes'));

-- The seed. Each tree root's approved versions, one object per root whose gate has released its
-- tree's work: a root holding a gate row with an approved version, at those versions, and a root
-- whose gate opened once (ever_opened) and holds no approved version now, at the empty object. A
-- row whose approval a changes request cleared (classify.ApplyDesignGateEvent) names no version and
-- is left out of the object, and a row a newer version closed keeps the approval it closed and is
-- in. A root whose gate never opened is left out: nothing of its tree has started, so each member's
-- first working phase takes its baseline (workflow's Engine.enter) and its architect's row is
-- taken at the tree's first opening (workflow's putGateTold), and neither is owed anything there.
-- It is a temporary table where one statement would hold a CTE, because the two inserts below read
-- the same set, and it is dropped when the transaction store.apply runs this migration in commits.
create temporary table design_news_seed on commit drop as
select issue as root,
  coalesce(jsonb_object_agg(step, approved_version) filter (where approved_version is not null), '{}'::jsonb) as versions
from design_gates
group by issue
having bool_or(approved_version is not null) or bool_or(ever_opened);

-- A holder row for each issue in the workflow (record.InWorkflow: its status is none the workflow
-- leaves, record.OutOfWorkflowStatuses, and its phase is short of done) whose tree root is in the
-- seed, at that root's versions: the work in flight stands on the version approved when it started
-- or since, so the upgrade owes nobody news of what the record already shows as taken. A done issue
-- gets none: no holder of it is owed anything.
--
-- An issue whose root's gate a changes request closed gets a row that knows no version, the empty
-- object, and not no row at all. What its holder took is unknown, and an empty object says exactly
-- that: record.OwedNews reads a step the row does not name as unknown, so once a person approves
-- again the reconcile reads the version it starts from in Dispatch's review history
-- (dispatch.Client's ApprovedVersionBefore). No row would say something else: that the issue's
-- first working phase is still to come, so its next phase entry would take the version approved
-- then as its baseline (record.Store.BaselineTold) and the news of what that approval changed
-- would be lost. A row already there is one that baseline leaves as it stands.
insert into design_news_told (issue, audience, versions, opening_owed, told_at)
select i.key, 'holder', seed.versions, false, now()
from issues i
join design_news_seed seed on seed.root = i.tree
where i.phase <> 'done' and i.status not in ('triage', 'icebox', 'backlog', 'done');

-- An architect row for each such root: the root of an issue the statement above seeded, which is a
-- tree root (issues.tree names one) in the seed, at those same versions, the empty object for a
-- root a changes request closed, for the same reason as its members': the tree's next opening is
-- not its first, so nothing else would write the row, and the architect is owed what that
-- approval changed from the version the review history names.
insert into design_news_told (issue, audience, versions, opening_owed, told_at)
select seed.root, 'architect', seed.versions, false, now()
from design_news_seed seed
where exists (select 1 from issues i where i.tree = seed.root
  and i.phase <> 'done' and i.status not in ('triage', 'icebox', 'backlog', 'done'));
