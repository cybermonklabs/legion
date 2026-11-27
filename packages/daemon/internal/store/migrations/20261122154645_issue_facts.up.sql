-- 20261122154645_issue_facts.up.sql — each issue's own facts, where the release before read them
-- off the tree it stored (dispatch://INFRA-4730, R17 and R22 of dispatch://INFRA-3320).
--
-- The issue's columns. retained_until is when a linger the release before converted ends for the
-- issue (record.Issue.RetainedUntil), carried_generation and carried_gate what a hand-over of that
-- release carried into the root it made, copied off handed_over_roots by the first boot's
-- conversion (record.Issue.CarriedGeneration, CarriedGate), pending_close a close Dispatch refused
-- while a child was not done, written again once every child is (record.Issue.PendingClose),
-- seen_seq the newest Dispatch sequence of the issue whose event the consumer has handled or
-- dropped (record.Issue.SeenSeq), and held_session_writes the status writes of sessions the
-- repair holds, in Dispatch's order, while the issue's Dispatch chain is unknown, judged and
-- cleared at the first pass that knows the chain (record.Issue.HeldSessionWrites, decision 24 of
-- dispatch://INFRA-4730), the empty list for none. pending_close is one nullable jsonb
-- of the four parts of one mark, its run, its phase entry, the close path that decided it and its
-- reason, because the mark is set and cleared whole: four columns could hold half of one, and the
-- kinds of close are the record's to name.
--
-- The claim's columns. launch_tree is the issue's gate root at the claim's last launch, which the
-- spawn spec and the registration's answer name, beside claims.tree, which stays the tree the
-- claim's issue stored, for the release before (supervise.Claim.LaunchTree, decision 13 of
-- dispatch://INFRA-4730). issue_epoch is the epoch of the claim's issue's own lifecycle it bound
-- to, and holds_gate whether the claim's issue held its own gate at its last launch
-- (supervise.Claim.IssueEpoch, HoldsGate). published_since is when the claim last began to hold
-- its role by its state and suspension source (supervise.HoldsRole), the rule the daemon
-- publishes its session as its role's holder by, and unpublished_since when it last stopped;
-- internal/store's upsertClaim writes each on that crossing alone, and the repair reads them to
-- judge a session's write at the time Dispatch recorded it (record.RoleHolderAt.PublishedAt,
-- decision 24 of dispatch://INFRA-4730). Both are nullable with no default, so the release
-- before's own claim writes, which name neither, still insert and update. This migration stamps
-- no claim and the first boot's conversion writes neither, so a claim no crossing has stamped
-- reads null, which the repair reads against the conversion's time (tree_conversions.converted_at).
--
-- The issue lifecycle. issue_lifecycles is the admission and cleanup barrier of one issue, column
-- for column what tree_lifecycles is for a tree (0033_tree_lifecycle), keyed by the normalized
-- project token claims use and by the issue: a claim binds an open epoch, a cleanup reserves it,
-- and only a fresh open after the cleanup confirmed opens the next one (internal/treelifecycle,
-- given the unit it guards). tree_lifecycles stays, and stays written: the release before binds
-- its claims to it.
--
-- The claims that stand at the upgrade keep running. Each issue a claim not retired names, whose
-- claim's tree has a lifecycle whose cleanup has not started, opens its own at epoch one under that
-- tree's authority, the workflow's where its claims' trees differ in it, and every claim of an issue
-- that now has a lifecycle binds epoch one; without that, the first relaunch of a live claim would
-- be refused as unbound. An issue of a tree whose cleanup is reserved opens none here: the first
-- boot's conversion moves that reservation onto each of its issues. Every claim whose issue is the
-- tree it stores, a claim on a root of the release before, holds its gate (holds_gate), retired or
-- not, so a root's architect is its tree's root from the upgrade on rather than from the
-- workflow's next start of it, which an operator's claim never gets. The project controller's
-- claim, on no issue, opens and binds nothing and holds no gate.
--
-- The conversion's record. tree_conversions holds, for each project the first boot converted, the
-- last numbered step of the conversion that committed, and when the last step ran (converted_at,
-- written by that step alone), so a boot after a crash starts at the next step.
--
-- The outbox. attempt_key and attempt_body are the request key and the body of the attempt a
-- status row with an option sent on Dispatch's status-change route, which no payload carries
-- (decision 6 of dispatch://INFRA-4730). retention_end is the kind that does for one issue, at its
-- retained_until, what the linger's expiry did for a tree (record.RetentionEnd). The check is one
-- list, so this migration names every kind, as 20261122154643 did: its thirty-two kinds, the design
-- news's design_changes and the basis seeds' pending_basis_seed and reparent_seed among them, with
-- retention_end added, main's merge_holds_changed (20261126110259) and main's three state kinds,
-- state_change, state_admission and state_boot (20261126163320), thirty-seven in all.
-- Migrate applies every unrecorded migration in version order, so the list with the higher version
-- is the last word on any database. 20261122154643 says its list is that word because of its
-- version; this file stands above it, so this list is the last word on a database an earlier build
-- of this branch migrated. Main's 20261126110259 and 20261126163320 stand above this file and name
-- retention_end too, so the list of 20261126163320 is the last word on a fresh database. On a
-- database main migrated, which recorded 20261126110259 and can hold a merge_holds_changed row, and
-- which, where it recorded 20261126163320 too, can hold a state row, this file is applied after
-- them and is the last word, so it names merge_holds_changed and the three state kinds: a list
-- without one of them would fail the upgrade over a row of it and refuse every later one. A list
-- here that left out design_changes, pending_basis_seed or reparent_seed would refuse every
-- reconcile of a tree's news and every basis or re-parent seed, and fail the upgrade of a database
-- that holds one pending.
--
-- The design news's reads over the Dispatch chain. 20261119203047_design_news reads what each
-- audience of a tree took (design_news_told_in) and whether a delivery's news still counts
-- (design_news_current) over issues.tree, the tree the record stored, which no decision of this
-- release reads (R22 of dispatch://INFRA-3320): a tree is its gate root and the issues in Legion's
-- work whose gate root it is on the Dispatch chain (record.Chains.GateTree), which no statement
-- reads. So the two stay for the release before, and this release reads through four functions of
-- its own, which the daemon gives the gate root and the issues of its tree. design_news_counts is
-- design_news_current with the gate root named: each item and opening a delivery carries names the
-- gate root it was rendered under (record.DeliveryNewsItem.Root, DeliveryOpening.Root) beside the
-- generation of its issue and of that root, and counts only while the record holds both at those
-- generations. design_news_credits_under and design_news_openings_under read a delivery's copy as
-- design_news_credits and design_news_openings do, through design_news_counts: those rendered under
-- the gate root named, or, where none is named (the claim store's fold), each under its own.
-- design_news_told_of is design_news_told_in over the issues named, counting only the credits
-- rendered under their gate root (record.Postgres.TreeTold). An item or an opening a build before
-- this one rendered names no gate root, so it counts for nothing and folds nothing, and its news
-- is told once more. A daemon of the release before calls none of the four.
--
-- A daemon of the release before names none of these columns and tables in any statement — it
-- neither selects the new columns nor inserts them, and an insert of its takes each one's default —
-- and refuses no retention_end row at the database, since this list admits the kind, and runs
-- none: none of its outbox lanes lists it (record.LaneKinds). There is no down file: migrations
-- here are forward-only.
--
-- A rollback of one project to the release before first runs, with its daemon stopped, the way
-- back below (R22 of dispatch://INFRA-3320, decision 15 of dispatch://INFRA-4730), and only then
-- the way back of each earlier migration a rollback across them names. The release before decodes
-- an outbox payload strictly (record.DecodeOutboxPayload, DisallowUnknownFields) and refuses a
-- kind and a notice kind it does not know, at every attempt. So the way back deletes the rows
-- that release cannot run: each retention_end row (the release before's own linger serves the
-- deadline it carries, from the linger_until the conversion left), each reparented notice and each
-- status-scope controller notice (told again by no one), and each status write that carries an
-- option of Dispatch's status-change route (written without its option it would break R24; a
-- person makes that write again). It takes off each payload key this release adds to a kind that
-- release reads, so those effects run after the rollback: the run and the phase entry each effect
-- names, the pod a stop, a cleanup, a save or a release names, the architect a notice names, the
-- row a status write waits for, the controller that asked for it and the Dispatch status its
-- record held when it was queued, and the parts of R18's seed beside the parent, the tree and the
-- words that release requires of it. It gives each issue the first boot's conversion made a root
-- of its own from a hand-over that had landed (a record with carried_generation set whose stored
-- tree is another issue's) what the release before's own re-admission and promotion would have
-- written, since that release re-admits only a record whose
-- key is its tree: the issue stores its own key as its tree, the tree it left where it still stands
-- at admitted and no tree where it has started (handed_over_from), its claims name that tree and
-- are bound to an open lifecycle of it, a handed_over_roots row says what it carried, and its
-- unfinished rows name that tree, at the issue's generation where they name a tree's generation.
-- It sets back each session write the repair holds on an issue whose Dispatch chain was unknown
-- (held_session_writes), since a take leaves the workflow through the engine and no statement can
-- make one, and the release before sets back every write it does not take: it prints each such
-- issue with its held writes and what the step does with them, queues one status write of the
-- issue in the release before's shape (record.ReassertStatusWrite: the record's status, observed
-- at the record's dispatch_status and the last held write's sequence, which that release writes
-- only while no one but the daemon has changed the status since) where Dispatch shows another
-- status and the record is one the release before sets a session's write back on: a record in the
-- workflow (a workflow status and a phase other than done, the rule correctStatuses skips the rest
-- by), and a root of its own tree that waits on a person with no linger (its sessionStatusWrite's
-- waiting root), whatever its status. It empties each list. Any other issue gets no row and is left
-- as Dispatch shows it. What that loses, the printed rows name: an architect
-- above's write, which that architect makes again once the release before runs, to be judged by
-- its own rule; a write on a child by a session with no claim of the issue's tree, which the
-- release before takes as a person's move and which that session or a person makes again; and a
-- write on an issue out of the workflow that the release before would have set back, which stays
-- on the board. It clears each of the project's claims' crossing times (published_since and
-- unpublished_since), which the release before neither reads nor writes, so a later upgrade reads
-- no crossing stamped before the rollback as a crossing after it. It deletes the project's record
-- of the conversion, which clears the state's
-- daemon.convertedAt, so the next boot on this release converts what the release before wrote in
-- between. Last it prints each issue that has a claim not retired with the tree it stores: a
-- Sandbox whose legion.dev/tree label differs from that tree is relabelled with it before the
-- release before
-- boots, since that release deletes a Sandbox by its tree label and sweeps one whose label is not
-- its issue's stored tree. Daemons of several projects can share the database, and each runs only
-- its own project's rows (split_part(issue, '-', 1)), so each statement names the project,
-- <PROJECT>, which the operator replaces with its key; the claims and the lifecycles name it by
-- its claim token, the key in lower case (claim.ProjectToken). The way back is the last paragraph
-- of this header (migrations.WayBack), and internal/store's
-- TestTheTreeConversionWayBackLeavesWhatTheReleaseBeforeReads runs it.
--
-- -- The rows of one project the release before cannot run.
-- delete from outbox where split_part(issue, '-', 1) = '<PROJECT>' and (kind = 'retention_end'
--   or (kind = 'notice' and payload->>'kind' = 'reparented')
--   or (kind = 'controller_notice' and payload->>'kind' = 'status-scope')
--   or (kind = 'dispatch_status' and (payload ? 'includeChildren' or payload ? 'unlinkChildren' or payload ? 'includeParent')));
-- -- The keys this release adds to the payloads of the kinds the release before reads.
-- update outbox set payload = payload - 'generation' - 'entry' - 'after' - 'controller' - 'dispatchStatus'
--   where kind = 'dispatch_status' and split_part(issue, '-', 1) = '<PROJECT>';
-- update outbox set payload = payload - 'generation' - 'entry' - 'to'
--   where kind in ('notice', 'controller_notice') and split_part(issue, '-', 1) = '<PROJECT>';
-- update outbox set payload = payload - 'generation' - 'entry'
--   where kind in ('dispatch_message', 'role_message', 'phase_ended', 'hook_prompt', 'update_ask', 'merge_forward',
--     'merge_queue_removal', 'claim_release') and split_part(issue, '-', 1) = '<PROJECT>';
-- update outbox set payload = payload - 'generation'
--   where kind in ('check_wake', 'check_look', 'check_bound') and split_part(issue, '-', 1) = '<PROJECT>';
-- update outbox set payload = payload - 'entry' - 'podUid' - 'readPodUid'
--     || case when coalesce(payload->>'readPodUid', '') <> '' then jsonb_build_object('podUid', payload->>'readPodUid') else '{}'::jsonb end
--   where kind in ('issue_suspend', 'issue_release') and split_part(issue, '-', 1) = '<PROJECT>';
-- update outbox set payload = case when coalesce(payload->>'launcher', '') = 'true'
--     then payload - 'entry' else payload - 'entry' - 'podUid' end
--   where kind = 'issue_bundle' and split_part(issue, '-', 1) = '<PROJECT>';
-- update outbox set payload = payload - 'podUid' where kind = 'supervise' and split_part(issue, '-', 1) = '<PROJECT>';
-- update outbox set payload = payload - 'generation' - 'entry' - 'oldParent' - 'phase' - 'slot' - 'steps'
--   where kind = 'reparent_seed' and split_part(issue, '-', 1) = '<PROJECT>';
-- -- Each root the conversion made of a hand-over that had landed, as the release before's re-admission leaves one.
-- insert into handed_over_roots (issue, generation, gate_carried)
--   select key, carried_generation, carried_gate from issues
--   where project = '<PROJECT>' and carried_generation <> 0 and tree <> key
--   on conflict (issue) do update set generation = excluded.generation, gate_carried = excluded.gate_carried;
-- insert into tree_lifecycles (project, tree, epoch, authority)
--   select lower('<PROJECT>'), key, 1, 'workflow' from issues
--   where project = '<PROJECT>' and carried_generation <> 0 and tree <> key
--   on conflict (project, tree) do update set epoch = tree_lifecycles.epoch + 1, authority = 'workflow',
--     cleanup_started = false, cleanup_confirmed_at = null, updated_at = now()
--   where tree_lifecycles.cleanup_started and tree_lifecycles.cleanup_confirmed_at is not null;
-- update claims c set tree = i.key, tree_epoch = l.epoch from issues i
--   join tree_lifecycles l on l.project = lower('<PROJECT>') and l.tree = i.key
--   where c.project = lower('<PROJECT>') and c.issue = i.key
--     and i.project = '<PROJECT>' and i.carried_generation <> 0 and i.tree <> i.key;
-- update outbox o set payload = o.payload || jsonb_build_object('tree', i.key)
--     || case when o.payload ? 'treeGeneration' then jsonb_build_object('treeGeneration', i.generation) else '{}'::jsonb end
--   from issues i where o.issue = i.key and o.payload ? 'tree'
--     and i.project = '<PROJECT>' and i.carried_generation <> 0 and i.tree <> i.key;
-- update issues set handed_over_from = case when phase = 'admitted' then tree else '' end, tree = key
--   where project = '<PROJECT>' and carried_generation <> 0 and tree <> key;
-- -- Each issue of one project holding session writes the repair held on an unknown chain, each write, and what the step does with them.
-- select i.key, w.session, w.status, w.seq,
--     case when i.dispatch_status <> i.status and ((i.phase <> 'done' and i.status not in ('triage', 'icebox', 'backlog', 'done'))
--         or (i.phase = 'awaiting_person' and (i.awaiting_ask is not null or i.awaiting_gate_version is not null) and i.key = i.tree and i.linger_until is null))
--       then 'set back to ' || i.status else 'left as Dispatch shows it, ' || i.dispatch_status end as step
--   from issues i cross join lateral jsonb_to_recordset(i.held_session_writes) as w(session text, status text, seq bigint)
--   where i.project = '<PROJECT>' and i.held_session_writes <> '[]'::jsonb order by i.key, w.seq;
-- insert into outbox (kind, issue, payload, attempts, next_at, last_error)
--   select 'dispatch_status', key, jsonb_build_object('kind', 'reassert', 'status', status, 'observedStatus', dispatch_status,
--       'observedSeq', (held_session_writes -> -1 ->> 'seq')::bigint), 0, now(), ''
--   from issues where project = '<PROJECT>' and held_session_writes <> '[]'::jsonb and dispatch_status <> status
--     and ((phase <> 'done' and status not in ('triage', 'icebox', 'backlog', 'done'))
--       or (phase = 'awaiting_person' and (awaiting_ask is not null or awaiting_gate_version is not null) and key = tree and linger_until is null));
-- update issues set held_session_writes = '[]'::jsonb where project = '<PROJECT>' and held_session_writes <> '[]'::jsonb;
-- -- The crossing times of the claims of one project, which the release before does not keep.
-- update claims set published_since = null, unpublished_since = null
--   where project = lower('<PROJECT>') and (published_since is not null or unpublished_since is not null);
-- -- The record of the conversion of one project.
-- delete from tree_conversions where project = '<PROJECT>';
-- -- Each issue of one project with a claim not retired, and the tree it stores, for the relabel of its Sandbox.
-- select key, tree from issues where project = '<PROJECT>'
--   and exists (select 1 from claims c where c.issue = issues.key and c.state <> 'retired') order by key;
alter table issues
    add column retained_until timestamptz,
    add column carried_generation bigint not null default 0 check (carried_generation >= 0),
    add column carried_gate boolean not null default false,
    add column pending_close jsonb,
    add column seen_seq bigint not null default 0 check (seen_seq >= 0),
    add column held_session_writes jsonb not null default '[]'::jsonb;

-- held_session_writes is empty on nearly every row, and every admission pass asks which issues of
-- its project hold any (record.Store.HeldSessionWriteIssues): this partial index answers that from
-- the few rows that hold some, rather than from a scan of every issue the project has recorded.
create index issues_held_session_writes on issues (project, key) where held_session_writes <> '[]'::jsonb;

alter table claims
    add column launch_tree text not null default '',
    add column issue_epoch bigint not null default 0 check (issue_epoch >= 0),
    add column holds_gate boolean not null default false,
    add column published_since timestamptz,
    add column unpublished_since timestamptz;

-- holds_gate is set on every claim the release before recorded on its tree's root, a claim whose
-- issue is the tree it stores: that release's own rule for a root (claim.IsTreeRoot), the one
-- 20261122154644 marks the issues by. supervise.Claim's treeRoot reads the mark, so a root's
-- architect left unmarked would be no tree's root: an operator's close of its tree would be
-- refused, and the stop and the close would act on it as on a member. The workflow's start writes
-- the mark again from the Dispatch chain at each launch (supervise.Machine.MarkLaunch), but the
-- workflow never starts an operator's claim, so nothing else would mark one. The project
-- controller's claim names no issue and stays unmarked.
update claims set holds_gate = true where issue <> '' and issue = tree;

create table issue_lifecycles (
    project text not null,
    issue text not null,
    epoch bigint not null check (epoch > 0),
    authority text not null check (authority in ('workflow', 'operator')),
    cleanup_started boolean not null default false,
    cleanup_confirmed_at timestamptz,
    updated_at timestamptz not null default now(),
    primary key (project, issue)
);

insert into issue_lifecycles (project, issue, epoch, authority)
select distinct on (c.project, c.issue) c.project, c.issue, 1, l.authority
from claims c join tree_lifecycles l on l.project = c.project and l.tree = c.tree
where c.issue <> '' and c.state <> 'retired' and not l.cleanup_started
order by c.project, c.issue, l.authority = 'workflow' desc;

update claims c set issue_epoch = 1
where exists (select 1 from issue_lifecycles l where l.project = c.project and l.issue = c.issue);

create table tree_conversions (
    project text primary key,
    step integer not null check (step > 0),
    converted_at timestamptz
);

alter table outbox
    add column attempt_key text,
    add column attempt_body jsonb;

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'retention_end', 'merge_holds_changed', 'state_change', 'state_admission', 'state_boot'));

-- design_news_counts is the one rule of which news of a delivery's copy still counts: an item or
-- an opening rendered for told_issue at told_generation, under the gate root told_root standing at
-- told_root_generation, counts only while the record holds both at those generations.
create function design_news_counts(told_issue text, told_generation bigint, told_root text, told_root_generation bigint)
  returns boolean language sql stable as $$
  select exists (select 1 from issues owner, issues root
    where owner.key = told_issue and owner.generation = told_generation
      and root.key = told_root and root.generation = told_root_generation)
$$;

-- design_news_credits_under is a delivery's copy as the credits it holds: each item rendered under
-- the gate root under, or under any where under is null, that still counts (design_news_counts), as
-- the link design_news_known reads ({step, from, to}), with the gate root it was rendered under,
-- which the claim store's fold holds to the gate root its issue stands under now.
create function design_news_credits_under(carried jsonb, under text)
  returns table (issue text, audience text, link jsonb, root text) language sql stable as $$
  select item.issue, item.audience, jsonb_build_object('step', item.step, 'from', item."from", 'to', item."to"), item.root
  from jsonb_to_recordset(coalesce(carried -> 'items', '[]'::jsonb))
    as item (issue text, audience text, step text, "from" int, "to" int, generation bigint, tree_generation bigint, root text)
  where (under is null or item.root = under) and design_news_counts(item.issue, item.generation, item.root, item.tree_generation)
$$;

-- design_news_openings_under is each opening a delivery's copy says opened, rendered under the gate
-- root under, or under any where under is null, that still counts (design_news_counts), with the
-- gate root it was rendered under. An opening the copy says closed is none of them, and one that
-- names no seq is returned with none, which matches no row.
create function design_news_openings_under(carried jsonb, under text)
  returns table (issue text, audience text, seq integer, root text) language sql stable as $$
  select opened.issue, opened.audience, opened.seq, opened.root
  from jsonb_to_recordset(coalesce(carried -> 'openings', '[]'::jsonb))
    as opened (issue text, audience text, seq int, closed boolean, generation bigint, tree_generation bigint, root text)
  where not coalesce(opened.closed, false) and (under is null or opened.root = under)
    and design_news_counts(opened.issue, opened.generation, opened.root, opened.tree_generation)
$$;

-- design_news_told_of is every told row of members, the issues of the tree of the gate root root,
-- as its audience took it: design_news_told_in's read, over the issues the daemon names and the
-- credits rendered under root (design_news_credits_under, design_news_openings_under).
create function design_news_told_of(root text, members text[])
  returns table (issue text, audience text, versions jsonb, opening_owed boolean, opening_seq integer, told_at timestamptz)
  language sql stable as $$
  with standing as (
    select delivery.carried_news from pending_task_deliveries delivery
    where delivery.confirmed_at is not null and not delivery.carried_ran and delivery.carried_news is not null),
  credit as (
    select credited.issue, credited.audience, credited.link
    from standing cross join design_news_credits_under(standing.carried_news, $1) credited
    where credited.issue = any($2)),
  cleared as (
    select opened.issue, opened.audience, opened.seq
    from standing cross join design_news_openings_under(standing.carried_news, $1) opened
    where opened.issue = any($2)),
  pair as (
    select told.issue, told.audience from design_news_told told where told.issue = any($2)
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
