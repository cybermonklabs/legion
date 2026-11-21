-- 20261120095128_issue_facts.up.sql — each issue's own facts, where the release before read them
-- off the tree it stored (dispatch://INFRA-4730, R17 and R22 of dispatch://INFRA-3320).
--
-- The issue's columns. retained_until is when a linger the release before converted ends for the
-- issue (record.Issue.RetainedUntil), carried_generation and carried_gate what a hand-over of that
-- release carried into the root it made, copied off handed_over_roots by the first boot's
-- conversion (record.Issue.CarriedGeneration, CarriedGate), parked_by_architect the mark that the
-- architect above parked the issue (record.Issue.ParkedByArchitect), pending_close a close
-- Dispatch refused while a child was not done, written again once every child is
-- (record.Issue.PendingClose), and seen_seq the newest Dispatch sequence of the issue whose event
-- the consumer has handled or dropped (record.Issue.SeenSeq). pending_close is one nullable jsonb
-- of the four parts of one mark, its run, its phase entry, the close path that decided it and its
-- reason, because the mark is set and cleared whole: four columns could hold half of one, and the
-- kinds of close are the record's to name.
--
-- The claim's columns. launch_tree is the issue's gate root at the claim's last launch, which the
-- spawn spec and the registration's answer name, beside claims.tree, which stays the tree the
-- claim's issue stored, for the release before (supervise.Claim.LaunchTree, decision 13 of
-- dispatch://INFRA-4730). issue_epoch is the epoch of the claim's issue's own lifecycle it bound
-- to, and holds_gate whether the claim's issue held its own gate at its last launch
-- (supervise.Claim.IssueEpoch, HoldsGate).
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
-- boot's conversion moves that reservation onto each of its issues. The project controller's
-- claim, on no issue, opens and binds nothing.
--
-- The conversion's record. tree_conversions holds, for each project the first boot converted, the
-- last numbered step of the conversion that committed, and when the last step ran (converted_at,
-- written by that step alone), so a boot after a crash starts at the next step.
--
-- The outbox. attempt_key and attempt_body are the request key and the body of the attempt a
-- status row with an option sent on Dispatch's status-change route, which no payload carries
-- (decision 6 of dispatch://INFRA-4730). retention_end is the kind that does for one issue, at its
-- retained_until, what the linger's expiry did for a tree (record.RetentionEnd). The check is one
-- list, so this migration names every kind, as 20261119185013 did: its thirty-one kinds with
-- retention_end added. Its version stands above 20261119185013, so this list is the last word on a
-- fresh database and on one an earlier build of this branch migrated.
--
-- A daemon of the release before names none of these columns and tables in any statement — it
-- neither selects the new columns nor inserts them, and an insert of its takes each one's default —
-- and refuses no retention_end row at the database, since this list admits the kind, and runs
-- none: none of its outbox lanes lists it (record.LaneKinds). There is no down file: migrations
-- here are forward-only.
alter table issues
    add column retained_until timestamptz,
    add column carried_generation bigint not null default 0 check (carried_generation >= 0),
    add column carried_gate boolean not null default false,
    add column parked_by_architect boolean not null default false,
    add column pending_close jsonb,
    add column seen_seq bigint not null default 0 check (seen_seq >= 0);

alter table claims
    add column launch_tree text not null default '',
    add column issue_epoch bigint not null default 0 check (issue_epoch >= 0),
    add column holds_gate boolean not null default false;

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
  'conflict_confirm', 'merge_forward', 'pending_basis_seed', 'reparent_seed', 'retention_end'));
