-- 20261120001432_intake_failures.up.sql — the intake's record of each message whose handling
-- fails, and the record of each pull request a new generation clears (dispatch://INFRA-1575).
--
-- intake_failures holds one row per failing message of a durable consumer, keyed by the consumer,
-- the stream sequence and the time JetStream stored the message, with the whole message: its
-- subject, its header block in the NATS/1.0 form (record.IntakeHeaders) and its payload, each byte
-- as it came, which is why both are bytea. A row is an open failure while given_up_at is null: the
-- intake writes it at the message's first failed delivery and deletes it when a later delivery
-- succeeds. A row given up is a record the operator or the controller replays or discards; closed_*
-- is its latest close, and discarded_* its discard, which a later replay's close keeps.
-- internal/record's intake_failures.go holds every statement on the table. Every row carries its
-- project, because the daemons of several projects can share one database.
--
-- cleared_pull_requests keeps, for each pull request whose row a new generation's clear deleted,
-- the time of the head the record last held, so a late opened event of that pull request at an
-- older head records nothing (record's clearGeneration, workflow's pullRequestOpened).
--
-- A rollback of one project to an earlier image first deletes two kinds of that project's row,
-- with its daemon stopped. Its intake-gave-up controller notices go, because the earlier image
-- decodes a notice strictly (record.DecodeOutboxPayload) and refuses this kind and its count and
-- give-ups at every attempt, as 20261113181500_architect_stalled_notices.up.sql says of its own
-- kind. Its open failures go, because the earlier image applies or drops those messages without
-- telling the table, so after a roll forward the sweep would give up as passed a message that
-- applied. The given-up records and both tables stay: the earlier image never reads them, and a
-- roll forward finds them in place under this migration's schema_version row, which Migrate does
-- not apply again. Each delete names the project, <PROJECT>, which the operator replaces with its
-- key, and leaves the rows of another project's daemon. The way back is the last paragraph of this
-- header (migrations.WayBack). docs/kubernetes.md carries the same block under "Rolling back the
-- release that gives up on intake messages", as the step an operator runs: internal/textcheck's
-- TestTheIntakeGiveUpWayBackIsOneTextInTheHeaderAndTheGuide holds the two to one text, and
-- internal/store's TestTheIntakeGiveUpWayBackLeavesWhatTheReleaseBeforeReads and
-- TestTheIntakeGiveUpWayBackDeletesOnlyTheRowsOfTheProjectThatRollsBack run this one.
--
-- delete from outbox where kind = 'controller_notice' and payload->>'kind' = 'intake-gave-up'
--   and split_part(issue, '-', 1) = '<PROJECT>';
-- delete from intake_failures where project = '<PROJECT>' and given_up_at is null;
create table intake_failures (
  id bigserial primary key,
  project text not null,
  consumer text not null,
  subject text not null,
  stream_seq bigint not null,
  -- The time JetStream stored the message, truncated to the microseconds PostgreSQL keeps, as
  -- intake's recordStreamPosition truncates it.
  stored_at timestamptz not null,
  headers bytea,
  payload bytea not null,
  -- Null for a message that did not decode.
  event_source text,
  event_id text,
  first_failed_at timestamptz not null,
  updated_at timestamptz not null,
  deliveries integer not null,
  ended_delivery integer not null default 0,
  last_error text not null default '',
  given_up_at timestamptz,
  give_up_reason text,
  -- The outbox id of the notice that counted the give-up, null when no controller was registered.
  notice_id bigint,
  replay_claimed_at timestamptz,
  closed_at timestamptz,
  closed_how text,
  closed_by text,
  discarded_at timestamptz,
  discarded_by text,
  discard_reason text,
  unique (consumer, stream_seq, stored_at),
  -- Each check is written so that a null in it makes the check false, never null, since a check
  -- whose expression is null passes.
  constraint intake_failures_give_up_reason check (
    (given_up_at is null and give_up_reason is null)
    or (given_up_at is not null and give_up_reason is not null
      and give_up_reason in ('bound', 'did not end', 'retention', 'undecodable', 'consumer moved past'))),
  constraint intake_failures_closed_how check (
    (closed_at is null and closed_how is null)
    or (closed_at is not null and closed_how is not null
      and closed_how in ('applied', 'already applied', 'no fact', 'discarded'))),
  constraint intake_failures_closed_given_up check (
    (closed_at is null and discarded_at is null) or given_up_at is not null),
  constraint intake_failures_discard check (
    (discarded_at is null and discarded_by is null and discard_reason is null)
    or (discarded_at is not null and discarded_by is not null and discard_reason is not null and discard_reason <> '')),
  constraint intake_failures_discarded_close check (
    closed_how is distinct from 'discarded' or discarded_at is not null)
);

-- The open give-ups of one project, which the state counts and lists and a bulk replay or discard
-- selects by consumer and stream sequence.
create index intake_failures_open_give_ups on intake_failures (project, consumer, stream_seq)
  where given_up_at is not null and closed_at is null;

create table cleared_pull_requests (
  repo text not null,
  number integer not null,
  head_updated_at timestamptz,
  cleared_at timestamptz not null,
  primary key (repo, number)
);

-- There is no down file: migrations here are forward-only. The way back above is what a rollback
-- runs.
