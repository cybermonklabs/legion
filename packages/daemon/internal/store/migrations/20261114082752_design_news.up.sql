-- 20261114082752_design_news.up.sql — what each holder of a tree's work took of its approved
-- design, the change sets between two approved versions, the questions of a document's review
-- history that could not be read, the news a delivery carries, and the outbox kind that reconciles
-- them (dispatch://INFRA-175).
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
-- was owed news, so the news task carries the opening in place of the role message. opening_seq
-- names the last settlement of the opening on the row (design_news_opening_settled, below). told_at
-- is when a write of the row's versions last touched it: the baseline or the fill that wrote it, or
-- the last raise, which sets it whether or not a version rose. It is null for a row that carries
-- only the owed opening. A row goes with its issue's generation (record.Postgres.clearGeneration),
-- beside the issue's gate rows.
--
-- design_changes holds what changed in one document from one version to another, read from
-- Dispatch (GET /api/v1/artifacts/{id}/changes). Two stored versions of a document never change,
-- so a recorded change set never goes stale, and keeping it is no copy of a value its owner can
-- move. state `read` is Dispatch's answer: decision_only, unchanged, and the sections and typed
-- blocks that differ. decision_only says the two versions differ in decision blocks alone, and
-- unchanged that they differ in nothing at all; each tells nobody anything of the work, and the
-- two are never true together, since Dispatch makes exactly one of the two flags or a named
-- section or block true of an answer. An entry of sections is a `##` section by its heading and
-- occurrence, or a run of the text outside every section, under the heading "" and occurrence 0
-- with the place Dispatch names it at (place, after_heading, after_occurrence): the jsonb keeps
-- them as the daemon writes them (record.SectionChange), so they need no column. state
-- `unavailable` is a pair Dispatch could not answer, a release without the route, a document
-- Dispatch no longer holds or a bounded run of failures, with the reason in reason; nothing was
-- read of it, so it is never unchanged, and the news then names the two versions and says why it
-- names no change. from_version 0 is a pair whose document had no approved version before
-- to_version: its holder took the step while none stood, or the review history was read and named
-- none.
--
-- design_history_unread holds each question of a document's review history (GET
-- /api/v1/artifacts/{id}/reviews) that the reconcile could not answer within its bounded attempts:
-- which version of artifact_id a person approved before to_version, asked for a holder whose told
-- row knows no version of that step. reason names the failed read. It is a state of its own, never
-- a version: the told row keeps no version of the step, the news says that version could not be
-- read and why, and the next reconcile asks the history again. The daemon attaches nothing new
-- from a row while a design_changes row of the tree that asks it is still to run, since that
-- reconcile replaces it; a delivery it attached the row to already carries the row's reason and
-- time, and is told by them. The read that answers fills the told row and deletes the question's
-- row in one write. So a failure Dispatch stops giving is never kept, and from_version 0 is never
-- written for a history nobody read.
--
-- pending_task_deliveries.design_news is the news items a delivery carries, each the issue, the
-- audience, the step, the document and the version pair, with the reason and time of the unread
-- review history an item whose earlier version could not be read was attached with, and the
-- opening it carries to each audience, with the opening_seq it was rendered under and whether the
-- gate had closed again at that send. The send renders it again from the record before each
-- prompt. It is null for a delivery that carries no news, which is every delivery the release
-- before this one queued.
--
-- pending_task_deliveries.carried_news is the copy of design_news that the prompt whose turn the
-- delivery's confirmation takes carried: written when Oh My Pi acknowledges a prompt of the task,
-- which is after the working-copy adoption and the identity mint succeeded, and fixed by the
-- confirmation. It is what that confirmation raises in design_news_told, never design_news, which
-- a later send may have rendered again with more in it. The write that takes the confirmation back
-- puts back what the row it replaces holds here, and so does the retirement of a confirmed delivery
-- whose turn's result never said the turn ran: the news goes back to owed, and a later task carries
-- it again. The result of a turn that ran clears it, so the confirmation's raise stands. It is
-- null wherever no prompt's raise is pending.
--
-- pending_task_deliveries.news_only says the delivery is a task queued for news of the approved
-- design alone (supervise.Delivery.NewsOnly), which is no task of its phase to any reader of a
-- claim's pending task. It is a column, written and read beside design_news, so that no writer of
-- the news can drop the mark.
--
-- The outbox kind design_changes is the row that runs that reconcile for one tree. The check is
-- one list, so it is rewritten whole naming every kind, as 20261110235320 did: the list of
-- 20261114031356, which stands below this one and added the two merge-queue kinds to the update
-- ask of 20261113082214, with design_changes added. Its version stands above 20261114031356 on
-- purpose. Migrate applies every unrecorded migration in version order, so the list with the
-- higher version is the last word on any database. Numbered below it, as this file first was
-- (20261113212423), this list would have run first on a fresh database, and the list of
-- 20261114031356, which does not name design_changes, would then have refused every reconcile
-- row. No release carries 20261113212423 and main never merged it, so no deployment's database
-- records it. Only a database that a build of this branch migrated before the forward merge does,
-- and such a database is a test's own: it would apply this file a second time and fail on the
-- first create table below, so make it again rather than upgrade it. design_history_unread,
-- design_changes.unchanged with its check, the function design_news_raised, the function
-- design_news_opening_settled with design_news_told.opening_seq, and the columns
-- pending_task_deliveries.carried_news and news_only were added to this file before any release
-- carried 20261114082752, so they are written here rather than in a migration of their own. A
-- database a build of this branch migrated before one of them was added records this version
-- without it, and it too is a test's own, to make again.
--
-- Rolling the image back to the release before this one needs no restore of the schema, but it
-- needs four statements first, run with this release's daemon stopped: that release refuses an
-- outbox payload member it does not know, keeps this release's news and its carried copy on a
-- delivery it writes over, and reads a task queued for news alone as its claim's phase task. The
-- statements, and why each is needed, are in docs/kubernetes.md under "Rolling back the release
-- that tells a holder of an approved new version". The outbox and pending_task_deliveries hold the
-- rows of every project that shares the database, so every daemon of one database moves to this
-- release, and back, together.
--
-- There is no down file: migrations here are forward-only.
create table design_news_told (
  issue text not null references issues (key) on delete cascade,
  audience text not null check (audience in ('holder', 'architect')),
  versions jsonb not null default '{}'::jsonb check (jsonb_typeof(versions) = 'object'),
  opening_owed boolean not null default false,
  opening_seq integer not null default 0,
  told_at timestamptz,
  primary key (issue, audience));

-- design_news_raised is the one statement of how a raise writes a told row's versions, held as
-- the row holds them and raised as the raise names them: each step raised takes the greater of the
-- two versions, a step the row does not know takes the one raised, and a step not raised keeps
-- what the row holds, so a raise never lowers a version and never forgets one. Both raises call it:
-- record.Postgres.RaiseTold, and the confirmation of a delivery that carried news (the claim
-- store's tellDesignNews), which is in a package that imports no record.
create function design_news_raised(held jsonb, raised jsonb) returns jsonb language sql immutable as $$
  select held || (select coalesce(jsonb_object_agg(step.key,
      greatest(step.value::int, coalesce((held ->> step.key)::int, step.value::int))), '{}'::jsonb)
    from jsonb_each_text(raised) step)
$$;

-- design_news_opening_settled is the one statement of how a told row's opening is settled. Every
-- writer of opening_owed calls it: the gate's opening, its close and its opened role message
-- (record.Postgres.SettleOpening and CloseOpenings, called from workflow's tellGateMoved), and the
-- confirmation of a delivery that carried the opening and the write that takes that confirmation
-- back (the claim store's tellDesignNews and untellDesignNews).
--
-- With carried null it is a new settlement of the opening: the gate opened while the audience is
-- owed news (owed true, which writes the row where there is none), or the gate closed, or the opened
-- role message told the audience (owed false). Each of the three takes the row's next opening_seq.
-- So an opening is named by the seq its row held when it became owed, and the close or the role
-- message that ends it moves the seq past it.
--
-- With carried set it is a delivery's settlement of the opening it carried under that seq: its
-- confirmation (owed false) and the take-back of that confirmation (owed true). Each acts only on a
-- row whose opening_seq is still carried. So a close and a reopening before the carried opening's
-- turn make a new opening, which that confirmation does not clear, and a role message or a close
-- since then is not undone by the take-back.
create function design_news_opening_settled(told_issue text, told_audience text, owed boolean, carried integer)
  returns void language plpgsql as $$
begin
  if carried is not null then
    update design_news_told set opening_owed = owed
      where issue = told_issue and audience = told_audience and opening_seq = carried;
  elsif owed then
    insert into design_news_told as told (issue, audience, opening_owed, opening_seq)
      values (told_issue, told_audience, true, 1)
      on conflict (issue, audience) do update set opening_owed = true, opening_seq = told.opening_seq + 1;
  else
    update design_news_told set opening_owed = false, opening_seq = opening_seq + 1
      where issue = told_issue and audience = told_audience;
  end if;
end
$$;

create table design_changes (
  artifact_id text not null,
  from_version integer not null check (from_version >= 0),
  to_version integer not null,
  state text not null check (state in ('read', 'unavailable')),
  decision_only boolean not null default false,
  unchanged boolean not null default false,
  sections jsonb not null default '[]'::jsonb check (jsonb_typeof(sections) = 'array'),
  blocks jsonb not null default '[]'::jsonb check (jsonb_typeof(blocks) = 'array'),
  reason text not null default '',
  read_at timestamptz not null,
  primary key (artifact_id, from_version, to_version),
  check (to_version > from_version),
  -- A pair Dispatch could not answer says why, and a pair it answered has no reason to give.
  check ((state = 'unavailable') = (reason <> '')),
  -- Only a pair Dispatch answered can be unchanged, and a decision-only one is not.
  check (not unchanged or (state = 'read' and not decision_only)));

create table design_history_unread (
  artifact_id text not null,
  to_version integer not null check (to_version > 0),
  -- A read that failed says why.
  reason text not null check (reason <> ''),
  read_at timestamptz not null,
  primary key (artifact_id, to_version));

alter table pending_task_deliveries add column design_news jsonb, add column carried_news jsonb,
  add column news_only boolean not null default false;

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'design_changes'));

-- The seed. Each tree root holding gate rows, with the approved version of each row that holds
-- one (versions) and whether it holds any (approved). A row whose approval a changes request
-- cleared (classify.ApplyDesignGateEvent) names no version and is left out of the object, so a
-- root a changes request closed holds the empty object, and a row a newer version closed keeps the
-- approval it closed and is in. Which roots and members take a row is the two inserts' rule below.
-- It is a temporary table where one statement would hold a CTE, because the two inserts below read
-- the same set, and it is dropped when the transaction store.apply runs this migration in commits.
create temporary table design_news_seed on commit drop as
select issue as root,
  coalesce(jsonb_object_agg(step, approved_version) filter (where approved_version is not null), '{}'::jsonb) as versions,
  bool_or(approved_version is not null) as approved
from design_gates
group by issue;

-- A holder row for each issue whose work has started: an issue in the workflow (record.InWorkflow:
-- its status is none the workflow leaves, record.OutOfWorkflowStatuses, and its phase is short of
-- done) whose phase has work, and whose tree root is in the seed, at that root's versions. The work
-- in flight stands on the version approved when it started or since, so the upgrade owes nobody
-- news of what the record already shows as taken.
--
-- A phase has work unless it is admitted, awaiting_person or done, and a held issue is read at the
-- phase it returns to, the one its hold left (held_from, as record.Issue.WorkingPhase reads it). An
-- issue at admitted has not started work of its own: a child waiting for its gate or its turn, or a
-- root whose architect writes the design or whose children do the work. It gets no row, so its
-- first working entry takes the version approved then as its baseline (workflow's Engine.enter,
-- record.Store.BaselineTold) and is owed nothing of an approval its work never stood on. A root in
-- awaiting_person waits on a person and no phase worker holds it (phase.RoleFor), and the holder of
-- a done issue is owed nothing. An issue held from a phase with work keeps its row: it has worked,
-- and it returns to that phase with what it took. One held from admitted gets none, as one standing
-- there does, since it has not started its work either: this release holds only a phase a role
-- works (workflow's claimFailed), but issues_held_from_check still takes admitted, so a row an
-- earlier release held there can stand at the upgrade.
--
-- This phase filter, and no read of design_gates.ever_opened, is what leaves out a tree whose gate
-- never opened. The gate holds the root's first phase and the start of every child it releases, so
-- no member of such a tree stands in a phase with work and none gets a row; its first opening takes
-- its architect's row (workflow's putGateTold), and each member's first working entry its own. The
-- filter is also more exact than reading the gate alone about whom a row is for: a member of an
-- opened tree that has not started gets no row, where an approved version or that mark gave it the
-- versions of an approval its work never stood on. And it keeps the seed off every column
-- 20261103183833_issue_slots adds, ever_opened among them: the first-boot tests of internal/daemon
-- build a store with every migration but that one and apply it after this one, to watch its
-- backfill (preReleaseStore).
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
where coalesce(case when i.phase = 'held' then i.held_from end, i.phase) not in ('admitted', 'awaiting_person', 'done')
  and i.status not in ('triage', 'icebox', 'backlog', 'done');

-- An architect row for each root whose gate released its tree's work, at the root's versions.
--
-- A root of an issue the statement above seeded takes one: work of its tree has started, so its
-- gate opened. For a root a changes request closed the row is the empty object, for the same
-- reason as its members': the tree's next opening is not its first, so nothing else would write
-- the row, and the architect is owed what that approval changed from the version the review
-- history names. Every row the table holds here is a holder row that statement wrote, since this
-- migration creates the table.
--
-- A root holding an approved version takes one too while an issue of its tree is in the workflow,
-- though none has started work yet: its gate opened, since a deployment that gates spec alone,
-- which every deployment this release upgrades does (dispatch://INFRA-175), holds an approval only
-- of a document that opened the gate. Its architect took those versions at that opening
-- (workflow's putGateTold), and with no row record.OwedNews would owe it each of them as a version
-- it does not know, which the sweep (the daemon's queueOwedDesignNews) would send as news.
--
-- A root holding no approved version and no issue at work holds a gate that never opened, and gets
-- none.
insert into design_news_told (issue, audience, versions, opening_owed, told_at)
select seed.root, 'architect', seed.versions, false, now()
from design_news_seed seed
where exists (select 1 from design_news_told told join issues i on i.key = told.issue
    where i.tree = seed.root)
  or (seed.approved and exists (select 1 from issues i where i.tree = seed.root
    and i.phase <> 'done' and i.status not in ('triage', 'icebox', 'backlog', 'done')));
