-- 20261119203047_design_news.up.sql — what each holder of a tree's work took of its approved
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
-- the last fold, which sets it whether or not a version rose. It is null for a row that carries
-- only the owed opening. A row goes with its issue's generation (record.Postgres.clearGeneration),
-- beside the issue's gate rows, and so does every credit of that issue a delivery holds: a credit
-- counts only in the run its news was rendered in (design_news_current, below).
--
-- What an audience took is never written as a bare version a later write must find again to undo.
-- It is read (design_news_told_in, below) as a function of what the row holds and of the credit
-- each delivery holds. A credit is a pair: the version a told prompt or a raise counted from (null
-- for one told with no earlier version) and the version it told. The version an audience took of a
-- step is the highest one reached from the row's versions through the credits that stand
-- (design_news_known): a credit reaches from any version at or above its own earlier one, so a
-- credit counted from a version that another credit gave, and that credit was then given back,
-- reaches nothing. A confirmed delivery's credit is its own row of pending_task_deliveries (below),
-- so the write that takes the confirmation back, or retires the delivery, ends exactly that credit,
-- whatever has been credited since, and a second confirmation of the same delivery replaces it. Once
-- the turn's result says the turn ran, the credit is final, and the write that records that folds
-- it into the row (design_news_folded): its versions rise where the credit reaches, and a credit
-- that does not reach yet waits in unreached, a JSON array of {step, from, to}, until a version it
-- counted from is reached. A work-unchanged raise (record.Postgres.RaiseTold) is folded the same
-- way, as a credit counted from the version its pair names. So no ordering of confirmations,
-- take-backs, raises and retirements can leave a version taken that a give-back should have undone.
-- An unfinal credit's openings count the same way: the row owes no opening that a standing credit
-- of the run the row stands in carried under the row's opening_seq.
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
-- audience, the step, the document, the version pair and the run the item was rendered in (the
-- generation of the issue and the generation of its tree's root, generation and tree_generation),
-- with the reason and time of the unread review history an item whose earlier version could not
-- be read was attached with, and the opening it carries to each audience, with the opening_seq and
-- the run it was rendered under and whether the gate had closed again at that send. The send
-- renders it again from the record before each prompt. It is null for a delivery that carries no
-- news, which is every delivery the release before this one queued.
--
-- pending_task_deliveries.carried_news is the copy of design_news that the prompt whose turn the
-- delivery's confirmation takes carried: written when Oh My Pi acknowledges a prompt of the task,
-- which is after the working-copy adoption and the identity mint succeeded, and fixed by the
-- confirmation, never design_news, which a later send may have rendered again with more in it.
-- While the row holds a confirmation and carried_ran is false, it is the delivery's credit: each
-- item and each opening it carries counts in what its audience took (design_news_told_in), and
-- nothing else is written. So the write that takes the confirmation back, or the retirement of the
-- delivery, ends that credit and no other, and the news goes back to owed for a later task to
-- carry. carried_ran says the turn's result said the turn ran with no provider error: the write
-- that sets it folds the copy into design_news_told (design_news_folded, design_news_opening_told),
-- so the credit is final and outlives the row. Both are null and false wherever no prompt's credit
-- is held. A clear of a generation leaves the copy as it stands, and the copy's items and openings
-- of each issue it covers then count for nothing and fold nothing (design_news_current): they were
-- rendered for a run that is over.
--
-- pending_task_deliveries.news_only says the delivery is a task queued for news of the approved
-- design alone (supervise.Delivery.NewsOnly), which is no task of its phase to any reader of a
-- claim's pending task. It is a column, written and read beside design_news, so that no writer of
-- the news can drop the mark.
--
-- The outbox kind design_changes is the row that runs that reconcile for one tree. The check is
-- one list, so it is rewritten whole naming every kind, as 20261110235320 did: the list of
-- 20261119003157, which stands below this one and added conflict_confirm and merge_forward to the
-- plan record's list of 20261117023932, itself the step record's list of 20261117000528 with
-- plan_record added, which is the role-holders list of 20261116165926 with step_record added, with
-- design_changes added. Its version stands above each of them on purpose. Migrate applies every
-- unrecorded migration in version order, so the list with the higher version is the last word on
-- any database. Numbered below any of them, this list would run first on a fresh database, and the
-- list above it, which does not name design_changes, would then refuse every reconcile row. A list
-- here that left out step_record, plan_record, conflict_confirm or merge_forward would refuse
-- every row of that kind, and fail the upgrade of a database that holds one pending. phase_ended
-- stays in the list for the reason 20261119003157 keeps it: no release since that one queues a
-- row of it, but the release before it does, and after a rollback to that release it runs on the
-- last list a migration wrote, which is this one. This file was numbered below such a list three
-- times, and moved above it at the forward merge that brought the list in: first 20261113212423,
-- below the merge-queue kinds' 20261114031356, then 20261114082752, below the role-holders kind's
-- 20261116165926, then 20261117133401, below the merge forward's 20261119003157. It stood above
-- the step record's 20261117000528 when that list came in, and above the plan record's
-- 20261117023932 when that one came in. No release carries any earlier version and main never
-- merged them, so no deployment's database records them. Only a database that a build of this
-- branch migrated before a forward merge does, and such a database is a test's own: it would
-- apply this file a second time and fail on the first create table below, so make it again rather
-- than upgrade it.
-- design_history_unread, design_changes.unchanged with its check, design_news_told.opening_seq and
-- unreached, the functions below, and the columns pending_task_deliveries.carried_news,
-- carried_ran and news_only were added to this file before any release carried it, so they are
-- written here rather than in a migration of their own. A database a build of this branch migrated
-- before one of them was added records this file's version without it, and it too is a test's
-- own, to make again.
--
-- 20261116165926 also creates outbox_role_holders_publish_issue_unique, a unique index on the
-- outbox's issue over role_holders_publish rows alone. Nothing below reads or writes that index or
-- a row of that kind, or a step_record, plan_record, conflict_confirm or merge_forward row; the
-- kind check below names role_holders_publish, step_record, plan_record, conflict_confirm and
-- merge_forward so that such a row stands through the rewrite and is still taken after it.
--
-- Rolling the image back to the release before this one needs no restore of the schema, but it
-- needs four statements first, run with this release's daemon stopped: that release refuses an
-- outbox payload member it does not know, keeps this release's news and its carried copy on a
-- delivery it writes over, and reads a task queued for news alone as its claim's phase task. The
-- statements, and why each is needed, are in docs/kubernetes.md under "Rolling back the release
-- that tells a holder of an approved new version". They clear carried_news, so the credit of a
-- confirmed delivery whose turn's result had not come goes with it, and this release tells that
-- news again after the roll forward; a credit already folded into design_news_told stands, and so
-- does every column the earlier release leaves alone (carried_ran on a row with no copy counts for
-- nothing). The outbox and pending_task_deliveries hold the rows of every project that shares the
-- database, so every daemon of one database moves to this release, and back, together.
--
-- There is no down file: migrations here are forward-only.
create table design_news_told (
  issue text not null references issues (key) on delete cascade,
  audience text not null check (audience in ('holder', 'architect')),
  versions jsonb not null default '{}'::jsonb check (jsonb_typeof(versions) = 'object'),
  opening_owed boolean not null default false,
  opening_seq integer not null default 0,
  told_at timestamptz,
  unreached jsonb not null default '[]'::jsonb check (jsonb_typeof(unreached) = 'array'),
  primary key (issue, audience));

-- design_news_known is the one statement of what an audience took of each step: the versions held
-- (a row's versions), raised through links, a JSON array of credits {step, from, to}. A credit
-- reaches a step whose version is at or above its from, or any step where its from is null (told
-- with no earlier version), and takes it to its to where that is higher; reached again, it changes
-- nothing. Links are read until none reaches further, so the order they are given in says nothing,
-- and a credit counted from a version that no standing credit or held version gives reaches
-- nothing. A step no credit names keeps what is held. Every reader of what an audience took reads
-- it through this (design_news_told_in), and so does every fold of a credit into the row
-- (design_news_folded).
create function design_news_known(held jsonb, links jsonb) returns jsonb language plpgsql immutable as $$
declare
  known jsonb := coalesce(held, '{}'::jsonb);
  link record;
  reached boolean := true;
begin
  while reached loop
    reached := false;
    for link in select credit.step, credit."from", credit."to"
        from jsonb_to_recordset(coalesce(links, '[]'::jsonb)) as credit (step text, "from" int, "to" int) loop
      if (link."from" is null or (known ->> link.step)::int >= link."from")
          and coalesce((known ->> link.step)::int < link."to", true) then
        known := known || jsonb_build_object(link.step, link."to");
        reached := true;
      end if;
    end loop;
  end loop;
  return known;
end
$$;

-- design_news_folded is the one write of a final credit into a told row: a work-unchanged raise
-- (record.Postgres.RaiseTold) and the credits of a delivery's copy whose turn's result said the
-- turn ran (design_news_credits, through the claim store's settleDesignNews). links are the
-- credits, as design_news_known reads them. The row is written where there is none. Its versions
-- take what they reach through its own unreached credits and links, and every credit that still
-- reaches no further than they do is kept in unreached, so a credit counted from a version an
-- unfinal credit gave waits for that credit to be folded too, and reaches nothing if that one is
-- given back. Folding a credit a second time changes nothing. told_at is set either way. The row is
-- read under its own lock, so a fold, a fill or a baseline of the same row that commits first is
-- read, never overwritten.
create function design_news_folded(told_issue text, told_audience text, links jsonb, at timestamptz)
  returns void language plpgsql as $$
declare
  waiting jsonb;
  known jsonb;
begin
  insert into design_news_told (issue, audience, told_at) values (told_issue, told_audience, at)
    on conflict (issue, audience) do nothing;
  select told.unreached || coalesce(links, '[]'::jsonb), told.versions into waiting, known
    from design_news_told told where told.issue = told_issue and told.audience = told_audience for update;
  known := design_news_known(known, waiting);
  update design_news_told set versions = known, told_at = at,
    unreached = coalesce((select jsonb_agg(distinct credit) from jsonb_array_elements(waiting) credit
      where coalesce((known ->> (credit ->> 'step'))::int < (credit ->> 'to')::int, true)), '[]'::jsonb)
    where issue = told_issue and audience = told_audience;
end
$$;

-- design_news_opening_settled is the one statement of how the gate settles a told row's opening:
-- the gate opened while the audience is owed news (owed true, which writes the row where there is
-- none), or the gate closed, or the opened role message told the audience (owed false). Its callers
-- are record.Postgres.SettleOpening and CloseOpenings, called from workflow's tellGateMoved. Each of
-- the three takes the row's next opening_seq, so an opening is named by the seq its row held when
-- it became owed, and the close or the role message that ends it moves the seq past it.
create function design_news_opening_settled(told_issue text, told_audience text, owed boolean)
  returns void language plpgsql as $$
begin
  if owed then
    insert into design_news_told as told (issue, audience, opening_owed, opening_seq)
      values (told_issue, told_audience, true, 1)
      on conflict (issue, audience) do update set opening_owed = true, opening_seq = told.opening_seq + 1;
  else
    update design_news_told set opening_owed = false, opening_seq = opening_seq + 1
      where issue = told_issue and audience = told_audience;
  end if;
end
$$;

-- design_news_opening_told is the one statement of how a delivery settles the opening it carried:
-- the fold of a final credit that said the gate opened under told_seq (design_news_openings,
-- through the claim store's settleDesignNews) owes it no longer, on a row that still holds that
-- seq. A close and a reopening before the turn make a new opening, which this leaves owed, and a
-- null seq matches no row, so a copy that names no seq clears nothing. Until its credit is final,
-- a delivery writes nothing: the read counts its standing credit instead (design_news_told_in), so
-- its take-back owes the opening again only where nothing else has settled it since.
create function design_news_opening_told(told_issue text, told_audience text, told_seq integer)
  returns void language sql as $$
  update design_news_told set opening_owed = false
    where issue = told_issue and audience = told_audience and opening_seq = told_seq
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
  add column carried_ran boolean not null default false, add column news_only boolean not null default false;

-- design_news_current is the one rule of which news of a delivery's copy still counts: an item or
-- an opening rendered for told_issue at the generation told_generation, in a tree whose root stood
-- at told_tree_generation (the item's generation and tree_generation, which the daemon's render
-- writes from issues.generation), counts only while the record holds the issue at that generation
-- and the root of the tree it stands in now at that tree generation. A clear of a generation
-- deletes what the holders of each issue it covers took (record.Postgres.clearGeneration), and
-- the write beside it raises one of the two: a child set back to todo (workflow's ReenterChild)
-- and an issue moved into another tree (workflow's moveIntoTree) raise the issue's own, and a root
-- set back to todo (admit's readmit) raises the root's alone, though its clear covers every issue
-- of its tree: the members keep their own generation. So news rendered for a run that is over
-- counts nothing, neither in the read of a standing credit (design_news_told_in) nor in the fold
-- of a final one (the claim store's settleDesignNews): the next run's row is written from that
-- run's own baseline and opening numbers, and the news stays owed to that run. The step, the
-- version numbers and the opening number are no proof of the run news was told to: a clear writes
-- the next run's row again from nothing, and a move into another tree takes another tree's
-- documents. News that names no generation, and news of an issue the record does not hold, counts
-- nothing either.
create function design_news_current(told_issue text, told_generation bigint, told_tree_generation bigint)
  returns boolean language sql stable as $$
  select exists (select 1 from issues owner join issues root on root.key = owner.tree
    where owner.key = told_issue and owner.generation = told_generation and root.generation = told_tree_generation)
$$;

-- design_news_credits is the one reading of a delivery's copy (pending_task_deliveries.carried_news)
-- as the credits it holds: each item that still counts (design_news_current), as the link
-- design_news_known reads ({step, from, to}).
create function design_news_credits(carried jsonb)
  returns table (issue text, audience text, link jsonb) language sql stable as $$
  select item.issue, item.audience, jsonb_build_object('step', item.step, 'from', item."from", 'to', item."to")
  from jsonb_to_recordset(coalesce(carried -> 'items', '[]'::jsonb))
    as item (issue text, audience text, step text, "from" int, "to" int, generation bigint, tree_generation bigint)
  where design_news_current(item.issue, item.generation, item.tree_generation)
$$;

-- design_news_openings is each opening a delivery's copy says opened that still counts
-- (design_news_current). An opening the copy says closed is none of them, and one that names no
-- seq is returned with none, which matches no row.
create function design_news_openings(carried jsonb)
  returns table (issue text, audience text, seq integer) language sql stable as $$
  select opened.issue, opened.audience, opened.seq
  from jsonb_to_recordset(coalesce(carried -> 'openings', '[]'::jsonb))
    as opened (issue text, audience text, seq int, closed boolean, generation bigint, tree_generation bigint)
  where not coalesce(opened.closed, false) and design_news_current(opened.issue, opened.generation, opened.tree_generation)
$$;

-- design_news_told_in is every told row of tree's issues as its audience took it: the one read of
-- design_news_told (record.Postgres.TreeTold), which every reader of what an audience took goes
-- through, so record.OwedNews, the reconcile's settlement and raise, a start and a send all weigh
-- the same answer. A row's versions are what design_news_known reaches from the row's versions
-- through its unreached credits and the credits of every confirmed delivery holding a copy whose
-- result has not yet said its turn ran (standing: design_news_credits of
-- pending_task_deliveries.carried_news, not carried_ran). Its opening is owed where the row owes
-- it and no such delivery carried that same opening, under the row's opening_seq, as opened
-- (design_news_openings). An audience a standing credit names while it has no row is read as a row
-- that knows only what the credits give and owes no opening. An item of an issue outside the tree
-- counts for nothing here.
create function design_news_told_in(tree text)
  returns table (issue text, audience text, versions jsonb, opening_owed boolean, opening_seq integer, told_at timestamptz)
  language sql stable as $$
  with member as (select owner.key from issues owner where owner.tree = $1),
  standing as (
    select delivery.carried_news from pending_task_deliveries delivery
    where delivery.confirmed_at is not null and not delivery.carried_ran and delivery.carried_news is not null),
  credit as (
    select credited.issue, credited.audience, credited.link
    from standing cross join design_news_credits(standing.carried_news) credited
    where credited.issue in (select member.key from member)),
  cleared as (
    select opened.issue, opened.audience, opened.seq
    from standing cross join design_news_openings(standing.carried_news) opened
    where opened.issue in (select member.key from member)),
  pair as (
    select told.issue, told.audience from design_news_told told where told.issue in (select member.key from member)
    union
    select credit.issue, credit.audience from credit)
  select pair.issue, pair.audience,
    design_news_known(coalesce(told.versions, '{}'::jsonb), coalesce(told.unreached, '[]'::jsonb)
      || coalesce((select jsonb_agg(credit.link) from credit
        where credit.issue = pair.issue and credit.audience = pair.audience), '[]'::jsonb)),
    coalesce(told.opening_owed, false) and not exists (select 1 from cleared
      where cleared.issue = pair.issue and cleared.audience = pair.audience and cleared.seq = told.opening_seq),
    coalesce(told.opening_seq, 0), told.told_at
  from pair left join design_news_told told on told.issue = pair.issue and told.audience = pair.audience
  order by pair.issue, pair.audience
$$;

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes'));

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
