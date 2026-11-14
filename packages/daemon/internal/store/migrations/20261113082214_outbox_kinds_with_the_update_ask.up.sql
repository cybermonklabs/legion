-- 20261113082214_outbox_kinds_with_the_update_ask.up.sql — the outbox kind that carries an
-- update round's ask to one role of the issue's pod (update_ask, INFRA-2981).
--
-- Before this change the ask was a task the machine queued on the claim. Now the sweep queues
-- one row for each role it asks, and the role-message lane publishes that row to the role's own
-- Envoy topic (record.UpdateAsk, the daemon's updateAsk). The row carries the text the sweep
-- built for the round, and it goes to a worker or to the issue's architect.
--
-- The check is one list, so the migration that sets it names every kind, as 20261106041346,
-- 20261108000545 and 20261110235320 did: whichever of two open branches merges last admits both
-- their kinds. The list is 20261110235320's twenty-one kinds with update_ask added. It drops the
-- constraint if it exists before adding it, so it is safe on a database at either branch.
--
-- There is no down file: migrations here are forward-only.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask'));
