-- 20261126223725_done_by_checks.up.sql — an issue whose work merged waits in the phase verifying
-- until the checks of its goal pass, and the records that wait reads (record's Issue, Defect,
-- CheckRead and the close read's and the close wait's outbox rows).
--
-- issues_phase_check takes verifying, the phase of an issue whose pull request merged and whose
-- close waits: for a check of its Verification document that Dispatch does not compute as a pass,
-- for its production check, for a defect, for an open next change, or for an issue beneath it. No
-- role works it and it holds no admission slot. issues_held_from_check stays without it: no claim
-- of a role works the phase, so no claim's failure holds an issue there. Every row the upgrade finds
-- keeps its phase and its status: an issue in merging stays in merging, and one done stays done.
--
-- The merge snapshot is the issue's newest merge on its own row: merge_number, merge_head (the
-- final reviewed head), merge_sha, merge_base, merge_repo (the repository the merge fact named) and
-- merged_at (GitHub's merged_at of that pull request). record.Store.PutMergeSnapshot is its one
-- writer, and it replaces a snapshot only with a later merge. PutIssue names none of the six, and
-- no clear of a generation touches them, so the snapshot outlives the pull request row a clear
-- deletes. A field nobody read stays null, and a snapshot is a pull request's number first: every
-- other field is null where the number is.
--
-- The wait's own columns are the record's (record.Issue): close_wait_since, since when the close
-- waits; close_wait, the stored wait that close-wait.md renders, an object of each item with its
-- state and since when it stands, the newest entry into verifying and the close's time;
-- closed_on_checks_at, when the issue closed because its checks passed; next_change, the generation
-- of the next change that is open, null for none; next_changes, the count of next changes started;
-- merger_completion_owed, that the merger's completion of the generation that merged has not landed;
-- and standing_told_at, when the owning architect was last told what still holds the close. Every
-- row the upgrade finds reads none of them: no issue waited in verifying before this release.
--
-- tree_generation is the tree's run: what fences each member's idle release, its suspension and its
-- design-news credits, and the tree's architect's run. It is the root's generation on every row the
-- upgrade finds, since until now the root's generation was the tree's run, and a root's next change
-- raises the root's generation alone. Only a rerun of the whole tree raises it. Its default, 1, is
-- the generation a root is first recorded at, for an insert that names none: the daemon's own insert
-- (PutIssue) always names it. design_news_current takes the item's audience, so an item told to the
-- architect of a tree root counts while the root's tree_generation equals the item's generation, and
-- every other item while its issue's generation does. design_news_credits and design_news_openings
-- pass each item's audience. step_run_witness's root_generation reads the root's tree_generation.
-- The three-argument design_news_current is dropped: nothing of this release calls it.
--
-- production_checks.relook_seq is the Dispatch sequence of the event that opened the newest look
-- after a close not_run, so a redelivery opens no second one. production_checks.generation is the
-- issue generation the check is of. Every check the upgrade finds reads generation 0, which binds no
-- generation: it runs on as the release before ran it, and never meets the close's production term.
-- An open production check of an issue an earlier release closed runs on as it ran.
--
-- defects holds each reason of a next change: a check Dispatch computes as a fail, a production
-- check's fail, or a defect a role reported, with the generation it was recorded in, the change that
-- carries it, and when that change merged or a withdrawal ended it. close_reads holds each issue's
-- newest read of its checks' states, with the moment the read began. The outbox takes close_read, the
-- read of an issue's checks for the judge of its close, and close_wait_publish, the write of its
-- close-wait.md. The kind list is the merge holds' of 20261126110259 with the two, written whole, so
-- this list is the last word on any database: its version stands above every list beneath it.
-- pending_task_deliveries.start_at is where a next change's first task starts the working copy (a
-- supervise start's StartAt), kept on the delivery so a restart between the queue and the send keeps
-- it. Every delivery the upgrade finds reads none.
--
-- There is no down file. This directory keeps none, so the way back is the two transactions that end
-- this header, its last paragraph (migrations.WayBack), run with every project's daemon on that
-- database stopped, before the earlier image is pinned. Block A runs once for each project, with
-- <PROJECT> its key, in any order, and block B once, after block A ran for every project.
--
-- Block A first prints the list that a person keeps, as two notices: the issues of the project that
-- stand in verifying, and the open defects of the project. psql prints each notice: keep both. It
-- then moves each issue of the project that stands in verifying to merging, with status retro. The
-- release before's boot sweep closes each of them at its merged pull request, which is the early
-- close this release ends, so after the roll forward a person reads the list and sets each listed
-- issue whose checks still do not pass to todo, which runs it again. It deletes the rows of the
-- project that the release before cannot decode, since it decodes a row strictly and fails such a row
-- at every attempt: the close_read and close_wait_publish rows, the notices and controller notices
-- of the kinds this release adds, the issue_branch rows whose payload holds from, and the supervise
-- rows whose payload holds startAt. An issue's close-wait.md stays in Dispatch as its last version
-- left it. It gives each issue_bundle, issue_release and issue_suspend row the outbox still holds
-- the tree generation the release before reads, its root's generation: after a way back a member of
-- a tree whose root ran a next change can be told one item of design news again, since its credits
-- name the old tree generation. Each statement that names a table or a column this migration adds
-- runs only where it still exists, so block A run again, or after block B, prints nothing it cannot
-- read and deletes that project's rows.
--
-- Block B refuses, and changes nothing, while any issue stands in verifying. Then it puts back the
-- SQL functions as the release before reads them, before it drops a column: design_news_current
-- with three arguments, design_news_credits and design_news_openings with their bodies of
-- 20261119203047, and step_run_witness with its body of 20261117004117. PostgreSQL records no
-- dependency of a language sql body on a column, so a body left reading tree_generation would fail at
-- run time after the drop, with nothing to refuse the drop. Only then does it drop the four-argument
-- design_news_current, the two tables and the new columns, put back outbox_kind_check and
-- issues_phase_check without the new values, and delete this migration's schema_version row, so the
-- roll forward applies this file again. The kind list it puts back is the merge holds' of
-- 20261126110259, the last list the release before writes, so merge_holds_changed stands while
-- close_read and close_wait_publish go, after block A deleted every row of both. An issue on a next
-- change is an ordinary generation, which the release before runs to its merge and closes there.
--
-- This migration was numbered 20261125004932 until the forward merge of 2026-11-26 brought in
-- 20261126110259, which stood above it. Its version now stands above every migration main carries
-- at that merge, so each list and each function it writes is the last word on a fresh database and
-- on a database main migrated alike: Migrate applies every unrecorded migration in version order,
-- and a migration of main below this one that wrote outbox_kind_check, issues_phase_check or one of
-- the four functions again would otherwise be applied after this one on a database main migrated. A
-- database a build of this branch migrated before this merge records the older version, and it is a
-- test's own, to make again.
--
-- -- Block A: once for each project, with <PROJECT> its key, in any order.
-- begin;
-- do $$ begin
--   if exists (select 1 from information_schema.columns where table_schema = current_schema() and table_name = 'issues' and column_name = 'close_wait_since') then
--     raise notice 'issues of <PROJECT> in verifying: %', (select coalesce(json_agg(listed), '[]') from (select key, close_wait_since, merge_number, merge_sha from issues where phase = 'verifying' and split_part(key, '-', 1) = '<PROJECT>' order by key) listed);
--   end if;
--   if to_regclass('defects') is not null then
--     raise notice 'open defects of <PROJECT>: %', (select coalesce(json_agg(listed), '[]') from (select issue, id, kind, text from defects where merged_at is null and withdrawn_at is null and split_part(issue, '-', 1) = '<PROJECT>' order by issue, id) listed);
--   end if;
-- end $$;
-- update issues set phase = 'merging', status = 'retro' where phase = 'verifying' and split_part(key, '-', 1) = '<PROJECT>';
-- delete from outbox where kind in ('close_read', 'close_wait_publish') and split_part(issue, '-', 1) = '<PROJECT>';
-- delete from outbox where kind in ('notice', 'controller_notice') and payload->>'kind' in ('close-waits', 'check-failed', 'check-unknown', 'defect-reported', 'next-change', 'close-wait-no-result', 'close-wait-standing', 'checks-unapproved', 'next-change-count', 'next-change-workspace', 'closed-check-failed') and split_part(issue, '-', 1) = '<PROJECT>';
-- delete from outbox where kind = 'issue_branch' and payload ? 'from' and split_part(issue, '-', 1) = '<PROJECT>';
-- delete from outbox where kind = 'supervise' and payload ? 'startAt' and split_part(issue, '-', 1) = '<PROJECT>';
-- update outbox o set payload = jsonb_set(o.payload, '{treeGeneration}', to_jsonb(root.generation)) from issues i join issues root on root.key = i.tree where i.key = o.issue and o.kind in ('issue_bundle', 'issue_release', 'issue_suspend') and split_part(o.issue, '-', 1) = '<PROJECT>';
-- commit;
-- -- Block B: once, after block A ran for every project.
-- begin;
-- do $$ begin if exists (select 1 from issues where phase = 'verifying') then raise exception 'an issue still stands in verifying: run block A for its project first'; end if; end $$;
-- create or replace function design_news_current(told_issue text, told_generation bigint, told_tree_generation bigint)
--   returns boolean language sql stable as $$
--   select exists (select 1 from issues owner join issues root on root.key = owner.tree
--     where owner.key = told_issue and owner.generation = told_generation and root.generation = told_tree_generation)
-- $$;
-- create or replace function design_news_credits(carried jsonb)
--   returns table (issue text, audience text, link jsonb) language sql stable as $$
--   select item.issue, item.audience, jsonb_build_object('step', item.step, 'from', item."from", 'to', item."to")
--   from jsonb_to_recordset(coalesce(carried -> 'items', '[]'::jsonb))
--     as item (issue text, audience text, step text, "from" int, "to" int, generation bigint, tree_generation bigint)
--   where design_news_current(item.issue, item.generation, item.tree_generation)
-- $$;
-- create or replace function design_news_openings(carried jsonb)
--   returns table (issue text, audience text, seq integer) language sql stable as $$
--   select opened.issue, opened.audience, opened.seq
--   from jsonb_to_recordset(coalesce(carried -> 'openings', '[]'::jsonb))
--     as opened (issue text, audience text, seq int, closed boolean, generation bigint, tree_generation bigint)
--   where not coalesce(opened.closed, false) and design_news_current(opened.issue, opened.generation, opened.tree_generation)
-- $$;
-- create or replace function step_run_witness(issue_key text, tree_key text) returns jsonb language sql stable as $$
--   select jsonb_build_object(
--     'at', coalesce(case when i.phase = 'held' then i.held_from end, i.phase),
--     'generation', i.generation,
--     'pass', (select p.pass from phases p where p.issue = i.key and p.role = 'implementer'),
--     'entries', (select jsonb_object_agg(p.role, p.entry) from phases p where p.issue = i.key),
--     'root_generation', root.generation,
--     'lingers', root.linger_until is not null)
--   from issues i left join issues root on root.key = tree_key
--   where i.key = issue_key
-- $$;
-- drop function if exists design_news_current(text, text, bigint, bigint);
-- drop table if exists close_reads;
-- drop table if exists defects;
-- alter table issues drop column if exists merge_number, drop column if exists merge_head, drop column if exists merge_sha, drop column if exists merge_base, drop column if exists merge_repo, drop column if exists merged_at, drop column if exists close_wait_since, drop column if exists close_wait, drop column if exists closed_on_checks_at, drop column if exists next_change, drop column if exists next_changes, drop column if exists merger_completion_owed, drop column if exists standing_told_at, drop column if exists tree_generation;
-- alter table production_checks drop column if exists relook_seq, drop column if exists generation;
-- alter table pending_task_deliveries drop column if exists start_at;
-- alter table outbox drop constraint if exists outbox_kind_check;
-- alter table outbox add constraint outbox_kind_check check (kind in ('dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed', 'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake', 'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release', 'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask', 'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record', 'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed', 'merge_holds_changed'));
-- alter table issues drop constraint if exists issues_phase_check;
-- alter table issues add constraint issues_phase_check check (phase in ('admitted', 'awaiting_person', 'awaiting_children', 'planning', 'implementing', 'testing', 'reviewing', 'accepting', 'retro', 'merging', 'done', 'held'));
-- delete from schema_version where version = 20261126223725;
-- commit;
alter table issues drop constraint issues_phase_check;
alter table issues add constraint issues_phase_check check (phase in (
  'admitted', 'awaiting_person', 'awaiting_children', 'planning', 'implementing', 'testing', 'reviewing', 'accepting',
  'retro', 'merging', 'verifying', 'done', 'held'));

alter table issues
  add column merge_number integer constraint issues_merge_number_check check (merge_number > 0),
  add column merge_head text,
  add column merge_sha text,
  add column merge_base text,
  add column merge_repo text,
  add column merged_at timestamptz,
  add column close_wait_since timestamptz,
  add column close_wait jsonb constraint issues_close_wait_check check (jsonb_typeof(close_wait) = 'object'),
  add column closed_on_checks_at timestamptz,
  add column next_change bigint constraint issues_next_change_check check (next_change >= 1),
  add column next_changes integer not null default 0 constraint issues_next_changes_check check (next_changes >= 0),
  add column merger_completion_owed boolean not null default false,
  add column standing_told_at timestamptz,
  add column tree_generation bigint constraint issues_tree_generation_check check (tree_generation >= 0),
  -- A snapshot is a pull request's number first: no other field of it stands without one.
  add constraint issues_merge_snapshot_check check (merge_number is not null
    or (merge_head is null and merge_sha is null and merge_base is null and merge_repo is null and merged_at is null));
update issues set tree_generation = generation;
alter table issues alter column tree_generation set not null, alter column tree_generation set default 1;

alter table production_checks
  add column relook_seq bigint not null default 0 constraint production_checks_relook_seq_check check (relook_seq >= 0),
  add column generation bigint not null default 0 constraint production_checks_generation_check check (generation >= 0);

alter table pending_task_deliveries add column start_at jsonb;

create table defects (
  id bigint generated always as identity primary key,
  issue text not null references issues (key) on delete cascade,
  generation bigint not null check (generation >= 0),
  kind text not null constraint defects_kind_check check (kind in ('check', 'production', 'defect')),
  text text not null check (btrim(text) <> ''),
  role text not null default '',
  head text not null default '',
  check_block text not null default '',
  result_id text not null default '',
  reported_at timestamptz not null,
  carried_by bigint check (carried_by >= 1),
  merged_at timestamptz,
  withdrawn_at timestamptz,
  withdraw_reason text not null default '',
  -- A reported defect names the head its role saw it at, and a check's reason its check and its
  -- deciding result.
  check (kind <> 'defect' or head ~ '^[0-9a-f]{40}$'),
  check (kind <> 'check' or (check_block <> '' and result_id <> '')),
  -- Only a change that carries a defect can merge it, and a withdrawal says why.
  check (merged_at is null or carried_by is not null),
  check ((withdrawn_at is null) = (withdraw_reason = '')));

-- The open defects of an issue: what the close's Defects term and the way back's list read.
create index defects_open on defects (issue) where withdrawn_at is null and merged_at is null;

create table close_reads (
  issue text primary key references issues (key) on delete cascade,
  generation bigint not null check (generation >= 0),
  read_at timestamptz not null,
  failure text not null default '',
  answer jsonb,
  recorded_at timestamptz not null,
  -- A read holds Dispatch's answer, or says why it failed.
  check ((failure = '') = (answer is not null)));

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'close_read', 'close_wait_publish'));

-- design_news_current is the one rule of which news of a delivery's copy still counts, now read on
-- the tree's run: an item or an opening rendered for told_issue in a tree whose run stood at
-- told_tree_generation counts while the root of the tree it stands in now holds that tree
-- generation. An item told to the architect of a tree root (told_audience architect, told_issue the
-- root) was rendered at the tree's generation, and counts while the root's tree_generation is
-- told_generation: a root's next change raises its own generation and not the tree's, and so does
-- not end its tree's architect's run. Every other item counts while its issue's generation is
-- told_generation, as before.
create function design_news_current(told_issue text, told_audience text, told_generation bigint, told_tree_generation bigint)
  returns boolean language sql stable as $$
  select exists (select 1 from issues owner join issues root on root.key = owner.tree
    where owner.key = told_issue and root.tree_generation = told_tree_generation
      and case when told_audience = 'architect' and owner.key = owner.tree then owner.tree_generation else owner.generation end = told_generation)
$$;

create or replace function design_news_credits(carried jsonb)
  returns table (issue text, audience text, link jsonb) language sql stable as $$
  select item.issue, item.audience, jsonb_build_object('step', item.step, 'from', item."from", 'to', item."to")
  from jsonb_to_recordset(coalesce(carried -> 'items', '[]'::jsonb))
    as item (issue text, audience text, step text, "from" int, "to" int, generation bigint, tree_generation bigint)
  where design_news_current(item.issue, item.audience, item.generation, item.tree_generation)
$$;

create or replace function design_news_openings(carried jsonb)
  returns table (issue text, audience text, seq integer) language sql stable as $$
  select opened.issue, opened.audience, opened.seq
  from jsonb_to_recordset(coalesce(carried -> 'openings', '[]'::jsonb))
    as opened (issue text, audience text, seq int, closed boolean, generation bigint, tree_generation bigint)
  where not coalesce(opened.closed, false) and design_news_current(opened.issue, opened.audience, opened.generation, opened.tree_generation)
$$;

drop function design_news_current(text, bigint, bigint);

-- step_run_witness's root_generation is the tree's run, which a root's re-admission raises: a
-- member's step the linger ended starts again at its promotion, and a root's next change, which
-- raises the root's own generation alone, ends no member's step.
create or replace function step_run_witness(issue_key text, tree_key text) returns jsonb language sql stable as $$
  select jsonb_build_object(
    'at', coalesce(case when i.phase = 'held' then i.held_from end, i.phase),
    'generation', i.generation,
    'pass', (select p.pass from phases p where p.issue = i.key and p.role = 'implementer'),
    'entries', (select jsonb_object_agg(p.role, p.entry) from phases p where p.issue = i.key),
    'root_generation', root.tree_generation,
    'lingers', root.linger_until is not null)
  from issues i left join issues root on root.key = tree_key
  where i.key = issue_key
$$;
