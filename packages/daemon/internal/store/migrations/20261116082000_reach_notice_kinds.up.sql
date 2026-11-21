-- 20261116082000_reach_notice_kinds.up.sql — the two outbox kinds of the notices this change adds:
-- reach_notice, the architect's notice of a claim of its tree out of reach, and
-- reach_controller_notice, the controller's notices of claims out of reach and of a claim locked
-- out of its worker stream (internal/record's OutboxKindReachNotice and
-- OutboxKindReachControllerNotice).
--
-- They are kinds of their own so that the release before them never runs one: that release decodes
-- a row strictly and knows neither those notice kinds nor their claim and unreachable fields, so
-- it would fail on such a row under notice or controller_notice at every attempt, and it runs a
-- row only through a lane that lists the row's kind. A rollback onto that release therefore leaves
-- each such row where it stands, and the next daemon of this one delivers it. Both ride the board
-- lane, as the other two notice kinds do; reach_notice holds the issue's pod as notice does (the
-- store's idleWaitingKinds).
--
-- That release starts on a database this file has migrated: it applies every embedded migration
-- its database has not recorded and asks nothing of a version it does not embed, and this file
-- changes only which kinds an outbox row may have, a list that admits every kind that release
-- writes. So a rollback needs no down migration, and none is written, since migrations here are
-- forward-only. scripts/e2e/rollback-notices.sh runs that release on such a database.
--
-- The check is one list, so it names every kind, as 20261114031356 did: its twenty-four, these
-- two, role_holders_publish, which 20261116165926_role_holders_publish adds, step_record, which
-- 20261117000528_outbox_kinds_with_the_step_record adds, plan_record, which
-- 20261117023932_outbox_kinds_with_the_plan_record adds, and conflict_confirm and merge_forward,
-- which 20261119003157_conflict_reads_and_merge_forward adds. Those versions stand above this one,
-- so a fresh database applies 20261119003157's list last, and that list names these two. A
-- database that recorded those four before this file applies this list last instead, over the
-- rows of their kinds its outbox holds, which a list without any of them would refuse, so the add
-- constraint below would fail and the daemon would not boot.
--
-- A database a build of this change migrated before these kinds existed can hold such a notice
-- under notice or controller_notice, where this release refuses to decode it, because a row's kind
-- and its notice's kind must agree (internal/record's DecodeOutboxPayload). The two updates move
-- each such row to its own kind. No release wrote one, so a deployment's database moves nothing.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'reach_notice', 'reach_controller_notice',
  'role_holders_publish', 'step_record', 'plan_record', 'conflict_confirm', 'merge_forward'));

update outbox set kind = 'reach_notice'
  where kind = 'notice' and payload->>'kind' = 'claim-out-of-reach';
update outbox set kind = 'reach_controller_notice'
  where kind = 'controller_notice' and payload->>'kind' in ('unreachable-claims', 'relaunch-ready',
    'relaunch-guard-held', 'relaunch-not-back', 'unreachable-exit', 'envoy-no-live-session',
    'worker-locked-out', 'claim-out-of-reach');
