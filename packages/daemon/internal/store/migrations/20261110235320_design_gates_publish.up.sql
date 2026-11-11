-- 20261110235320_design_gates_publish.up.sql — the outbox kind that publishes the documents an
-- issue's design-gate steps record onto its Dispatch issue row (design_gates_publish).
--
-- Dispatch holds, per issue, which document each step's gate recorded, and its write advice and
-- its CLI name that document from this record alone. The daemon is what writes it, since the
-- record of a gate is the daemon's. The publish rides the outbox like every other Dispatch write,
-- so a Dispatch that is down, or one whose release has not got the route yet, fails no
-- transaction that changed a gate and the row is retried until it lands. The row belongs to its
-- issue, unlike the gated_steps_publish of 20261106041346, whose issue column holds the bare
-- project key: this one's is an issue key, so the row waits on that issue's board lane among its
-- other Dispatch writes.
--
-- The check is one list, so the migration that sets it names every kind, as 20261106041346 and
-- 20261108000545 did: whichever of two open branches merges last admits both their kinds.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_wake', 'check_look', 'check_bound',
  'phase_ended', 'issue_bundle', 'issue_release', 'gated_steps_publish', 'claim_release',
  'design_gates_publish'));

-- One publish row per issue, which is what lets the enqueue be an upsert
-- (record.Postgres.EnqueueDesignGates): the row waits on its lane until Dispatch takes it, and
-- every gate change inside that window replaces the set it carries rather than queueing beside
-- it, since the whole set is what the route is sent. The index holds design_gates_publish rows
-- alone, so no other kind's insert conflicts on it; the project publish's index is on the same
-- column, but its rows are keyed by a bare project key, which no issue key can equal. The kind
-- is new in this migration, so there is nothing to collapse first.
create unique index outbox_design_gates_publish_issue_unique
  on outbox (issue)
  where kind = 'design_gates_publish';
