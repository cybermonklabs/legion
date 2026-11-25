-- 20261117023932_outbox_kinds_with_the_plan_record.up.sql — the outbox kind that writes the plan one
-- run of an issue carries out onto Dispatch (plan_record), when the run's work starts.
--
-- The row rides the record lane (internal/record's LaneRecord) beside the step records, so a
-- Dispatch that is slow to take a plan, or that backs one off until it has the route, holds no
-- status write, notice or start of the issue, and it does not hold the issue's pod (it is not among
-- the store's idleWaitingKinds): a record is history, and the work it records never waits for it.
--
-- The check is one list, so the migration that sets it names every kind, as 20261117000528 did:
-- its twenty-six kinds, which are the kinds the daemon writes, role_holders_publish and
-- step_record among them, and this one. Its version stands above 20261117000528 on purpose:
-- Migrate applies every unrecorded migration in version order, so the list with the higher
-- version is the last word on any database, and a list that left out a kind below it would refuse
-- every row of that kind and fail the upgrade of a database that holds one pending. A rollback
-- leaves this list in the check, and the way back below restores none: the list names every kind
-- an earlier release writes, so it refuses nothing that release writes (docs/kubernetes.md,
-- "Rolling back the release that rates each plan").
--
-- A rollback of one project to an earlier image first deletes two kinds of that project's row,
-- with its daemon stopped: its plan_record rows, and its step_record rows that carry the
-- rating a retro gave the plan, which the release that records each step decodes strictly and
-- refuses at every attempt. Daemons of several projects can share the database, and each runs only
-- its own project's rows (split_part(issue, '-', 1)), so one project can roll back while another
-- stays on this release: each delete names the project, <PROJECT>, which the operator replaces
-- with its key, and leaves the rows another project's daemon still sends. The way back is the last
-- paragraph of this header (migrations.WayBack). docs/kubernetes.md carries the same block under
-- that heading, as the step an operator runs, and the two say the same thing: internal/textcheck's
-- TestThePlanRatingsWayBackIsOneTextInTheHeaderAndTheGuide holds them to one text, and
-- internal/store's TestThePlanRatingsWayBackLeavesWhatTheReleaseBeforeReads and
-- TestThePlanRatingsWayBackDeletesOnlyTheRowsOfTheProjectThatRollsBack run this one.
--
-- -- The writes of the runs' plans and of the retros' ratings onto Dispatch, of one project.
-- delete from outbox where kind = 'plan_record' and split_part(issue, '-', 1) = '<PROJECT>';
-- delete from outbox where kind = 'step_record' and payload->>'rating' is not null
--   and split_part(issue, '-', 1) = '<PROJECT>';
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record'));
