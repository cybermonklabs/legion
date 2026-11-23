-- 20261122104755_spec_gate_exemption.up.sql — the rule each design gate started under, the version
-- that opened a gate with no person, the version of the Verification document a gate's change
-- request was read at, the daemon's last reading of each issue's ending condition, and the outbox
-- kind that reads that condition again (R23, R24, R25, R26).
--
-- design_gates.rule is record.DesignGate.Rule: `approval`, the rule of a person's approval, which
-- every row the table already holds takes, since no release before this one opened a gate any other
-- way, or `change_request`, the rule the first registration of a spec-source root's spec step fixes
-- (workflow's gateRegistered). A later registration keeps the stored rule (R24). The column has a
-- default, so the release before, whose insert names neither column (record's PutGate), still
-- writes its rows, each under the rule of a person's approval.
--
-- design_gates.exempt_version is record.DesignGate.ExemptVersion: the version of the document that
-- Dispatch read as proposing no statement, component or interface and changing no approved check,
-- which opens a gate under the rule of a change request with no person (R23, record.GateExempt).
-- It is null where no version opened the gate that way, which is every row the table already holds.
--
-- design_gates.verification_version is record.DesignGate.VerificationVersion: the newest version of
-- the issue's Verification document a read of the gate's change request saw, 0 where no read saw
-- one, which is every row the table already holds. Under the rule of a change request the request
-- reads its checks from that document, so a version of it that changes an approved check closes a
-- gate open by exemption (the workflow's verificationVersion), and a read older than it opens
-- nothing (the workflow's exemptAt).
--
-- spec_endings is record.SpecEnding: one row per issue in merging whose tree's spec gate is open by
-- exemption, holding the pull request head the ending condition was last read at, the state
-- Dispatch answered (`ready` or `waiting`) and each reason code the tree root's architect was told
-- of at that head (D10). The merger's start fence and the outbox's spec_ending_read row write it
-- (workflow's specEndingRead), and the deliveries a released merger is woken for wait while it
-- waits at the pull request's current head (the daemon's deliverHeld). A row goes with its issue's
-- record.
--
-- spec_ending_read is the outbox kind of record.SpecEndingRead: the read of the ending condition
-- that a head move of a merging member's pull request, a spec.outcomes_recorded event of an issue of
-- the tree, and a version or an approval of the root's spec document queue for every member in
-- merging. The kind check is written again with it added to the list
-- 20261119003157_conflict_reads_and_merge_forward.up.sql wrote.
--
-- The release before decodes an outbox row strictly and refuses a kind it does not know
-- (record.DecodeOutboxPayload), so a spec_ending_read row left queued fails on every attempt and
-- never finishes: there is no attempt cap and no dead letter. Nor does it decode the two notice
-- kinds this release adds: spec-ending-waits, the architect's notice of each reason the ending
-- condition does not hold at a head, and spec-request-stale, the notice of a change request whose
-- approval Dispatch refused as stale, which reaches the issue's owner as outbox kind 'notice' and the
-- controller as outbox kind 'controller_notice' where the owner is gone (D8). A 'notice' row of
-- either kind costs more, as 20261109060737_drop_awaiting_slot_notice.up.sql says of the row it
-- deletes: while its backoff holds next_at ahead of now it is one of the rows
-- record.Store.WaitingNotices returns, and daemon.pacedCatchUps returns its decode error, so GET
-- /legion/v1/state answers an error for the whole project.
--
-- After a rollback the release before reads every gate under the rule of a person's approval, so a
-- gate open by exemption reads closed, and its tree waits for a person's approval of the spec. That
-- is today's gate, the one the release before has always kept.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with this release's daemon stopped and before the release
-- before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1.
--
-- The outbox delete takes the spec_ending_read rows and the notices of the two kinds this release
-- queued and had not sent when the daemon stopped, the architect's and the controller's, as the way
-- back of 20261120042902_claim_agent_resume_generation.up.sql takes its session-forgotten notices.
-- A sent row leaves nothing, since the outbox deletes a row once it is delivered, so these are the
-- only ones standing. What the delete costs is those tellings and those reads: the architect is not
-- told of a reason told by no earlier notice, and the controller of a stale request whose owner was
-- gone. The kind check is written back as 20261119003157 wrote it, which refuses the kind again.
--
-- The drops and the delete of this migration's row go together. The release before needs none of
-- them to run, since it names no column and neither the table nor the kind: the delete is what
-- makes the version the store reports, its highest recorded row (store.SchemaVersionTx), that
-- release's own again. All are owed to the roll-forward. With the columns and the table dropped and
-- the row deleted, store.apply finds no row of this version and runs this file again, which adds
-- the columns at their defaults and the table empty. What the drops cost is every rule, every
-- exemption, every Verification version and every reading recorded: after the roll-forward every
-- gate is under the rule of a person's approval, and a root's next registration of its spec step
-- under a generation the rollback did not clear keeps that rule.
--
-- begin;
-- delete from outbox where kind = 'spec_ending_read';
-- delete from outbox where kind in ('notice', 'controller_notice') and payload->>'kind' in ('spec-ending-waits', 'spec-request-stale');
-- alter table outbox drop constraint if exists outbox_kind_check;
-- alter table outbox add constraint outbox_kind_check check (kind in ('dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed', 'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake', 'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release', 'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask', 'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record', 'conflict_confirm', 'merge_forward'));
-- drop table if exists spec_endings;
-- alter table design_gates drop column if exists rule, drop column if exists exempt_version, drop column if exists verification_version;
-- delete from schema_version where version = 20261122104755;
-- commit;
alter table design_gates
  add column rule text not null default 'approval' check (rule in ('approval', 'change_request')),
  add column exempt_version integer check (exempt_version > 0),
  add column verification_version integer not null default 0 check (verification_version >= 0);

create table spec_endings (
  issue text primary key references issues (key) on delete cascade,
  head text not null check (head <> ''),
  state text not null check (state in ('ready', 'waiting')),
  told text[] not null default '{}',
  read_at timestamptz not null default now());

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'spec_ending_read'));
