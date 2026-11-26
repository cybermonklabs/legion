-- 20261120144241_intake_failures.up.sql — the intake's record of each message whose handling
-- fails, the record of each pull request a new generation clears, and what a pull request
-- remembers of the heads it settled (dispatch://INFRA-1575).
--
-- intake_failures holds one row per failing message of a durable consumer, keyed by the consumer,
-- the stream sequence and the time JetStream stored the message, with the whole message: its
-- subject, its header block in the NATS/1.0 form (record.IntakeHeaders) and its payload, each byte
-- as it came, which is why all three are bytea: a NUL or a byte that is no UTF-8, which a text
-- column refuses, is the publisher's to send, and a replay decodes the subject and the payload the
-- delivery decoded. A row is an open failure while given_up_at is null: the intake writes it at the
-- message's first failed delivery and deletes it when a later delivery succeeds. A row given up is
-- a record the operator or the controller replays or discards; closed_* is its latest close, and
-- discarded_* its discard, which a later replay's close keeps. A row is as large as its message,
-- which the NATS server bounds at its max_payload (1 MB unless its configuration raises it): the
-- 27 envelopes captured in internal/intake/testdata are 369 to 1,231 bytes (wc -c), and a CI
-- settlement grows with the checks it names. A given-up record is kept for good, with no
-- retention: a replay or a discard closes it and keeps it, as history. The growth is an estimate
-- from two measurements: the 30 days to 2026-11-19 gave 7,449 messages that this change would give
-- up, 4,292 of them on one day from one defect since fixed (cybermonklabs/legion#4315), which at
-- the largest captured envelope is about 9 MB. The failure path, the state and the controller's
-- notice read no payload and no headers (record's intakeGiveUpColumns); only a replay's claim of
-- a record reads them.
-- internal/record's intake_failures.go holds every statement on the table. Every row carries its
-- project, because the daemons of several projects can share one database.
--
-- cleared_pull_requests keeps, for each pull request whose row a new generation's clear deleted,
-- the newest lifecycle clock the record applied (head_updated_at), so a late opened or reopened
-- event of that pull request no newer than that clock records nothing, whatever its head
-- (record's clearGeneration, workflow's pullRequestOpened). It keeps an issue too, the issue of
-- whatever wrote the row last: the issue whose row the latest clear deleted, or, where the one-time
-- rename pass has since merged a moved row into this row, the moved row's issue. That pass reads
-- and writes the row by it with the issue's other rows that name a repository (record's
-- RenameCandidates and RenameIssueRepository).
--
-- pull_request_settled_heads keeps, for each of the last heads a CI settlement of a pull request
-- applied on, that head's check fence as the last settlement applied on it left it: the newest run
-- of each check, the newest commit status of each context, the checks that came to failed and to
-- cancelled, and the listener's generation and snapshot, one row for each head, at most 16 heads
-- of each kind (classify's maxSettledHeads). Kind pull_request is the pull request's own memory
-- (record.PullRequest.SettledHeads), and kind head_run the memory its open head run was anchored
-- with (record.HeadRun.Settled), which the run's replay starts from; position is the entry's place
-- in its list, oldest first. The check fence (check_runs, statuses and the columns beside them)
-- holds one head's and is emptied by a reopen and by a new generation's clear, and a settlement of
-- another head moves it there; a settlement of a head the fence does not stand at is weighed
-- against this table's entry for it, so an earlier settlement of a head the branch came back to
-- is stale, as it is against the fence (classify.SettlementFor).
--
-- It is a table of its own, rather than a column of pull_requests with a copy inside head_run, for
-- its size. One head's fence of 63 check runs (this change's own pull request at c8fecac2) is 6,638
-- bytes of JSON, 7,148 as jsonb prints it and 1,725 as stored (pg_column_size, compressed). Written
-- with the head in each of its runs it would be 9,794 bytes of JSON, so a run or a status of the
-- row's head is stored with no head, and the read stamps it back (record's storedFence). A pull
-- request remembering 16 such heads holds 27,600 bytes as stored (114,368 as jsonb prints them),
-- and 55,200 (228,736) in its 32 rows while a head run is open. As a column, every read of a pull
-- request would load it - the state's read of each issue and the list reads among them - and every
-- write would write it whole. Here the state and the list reads load none of it (the state reads
-- every issue's pull request with record's PullRequestsOf). Only the three whole reads of one pull
-- request load it, up to 32 rows (record's loadSettledHeads), and only the reads a fact's handling
-- may write back, or that weigh a settlement against the memory as the write would (workflow's
-- wakeOnSettlement), make them: every other read of one pull request reads its row alone
-- (record's PullRequestRow, PullRequestRowByNumber and PullRequestRowByBranch). A write sends the
-- memory in one statement that writes only the rows whose place or fence changed (record's
-- putSettledHeads). An earlier daemon never reads or writes the table: a pull request row it
-- deletes takes that pull request's rows with it (on delete cascade), and the way back keeps the
-- rest.
--
-- A rollback of one project to an earlier image first deletes two kinds of that project's row,
-- with its daemon stopped. Its intake-gave-up controller notices go, because the earlier image
-- decodes a notice strictly (record.DecodeOutboxPayload) and refuses this kind and its count and
-- give-ups at every attempt, as 20261113181500_architect_stalled_notices.up.sql says of its own
-- kind. Its open failures go, because the earlier image applies or drops those messages without
-- telling the table, so after a roll forward the sweep would give up as passed a message that
-- applied. The given-up records and the three tables stay: the earlier image never reads them,
-- and a roll forward finds them in place under this migration's schema_version row, which Migrate
-- does not apply again. Each delete names the project, <PROJECT>, which the operator replaces with
-- its key, and leaves the rows of another project's daemon. The way back is the last paragraph of
-- this header (migrations.WayBack). docs/kubernetes.md carries the same block under "Rolling back
-- the release that gives up on intake messages", as the step an operator runs: internal/textcheck's
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
  subject bytea not null,
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

-- The open failures of one consumer, which the failure sweep lists at or below its ack floor once
-- a minute, and a consumer made again lists whole (record's ListPassedFailures).
create index intake_failures_open_failures on intake_failures (project, consumer, stream_seq)
  where given_up_at is null;

create table cleared_pull_requests (
  repo text not null,
  number integer not null,
  issue text not null,
  head_updated_at timestamptz,
  cleared_at timestamptz not null,
  primary key (repo, number)
);

-- The clock read of a pull request a clear deleted (record's ClearedPullRequestClock) compares the
-- repository case-insensitively, as 20261102095707_pull_request_repo_case_reads.up.sql indexes
-- pull_requests for the same read.
create index cleared_pull_requests_lower_repo_number on cleared_pull_requests (lower(repo), number);

create table pull_request_settled_heads (
  issue text not null references pull_requests (issue) on delete cascade,
  kind text not null check (kind in ('pull_request', 'head_run')),
  position smallint not null,
  head text not null,
  fence jsonb not null,
  primary key (issue, kind, head)
);

-- There is no down file: migrations here are forward-only. The way back above is what a rollback
-- runs.
