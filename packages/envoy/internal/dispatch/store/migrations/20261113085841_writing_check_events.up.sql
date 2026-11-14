-- 20261113085841_writing_check_events.up.sql — one row for each write the writing check decided.
--
-- INFRA-3231: the server reads the prose of an agent's write before it stores it and refuses a
-- write that breaks a rule of the check (api/writing_check.go). This table records what the check
-- decided for each write it saw: refused, forced through with override_writing_check, written
-- clean, let through because the reader failed (the reader's fail-open outcomes), or not read at
-- all because the writer is a person, a project's daemon or Dispatch itself, or because the
-- upload is a file the check does not read. A person reads it with the SQL in
-- cmd/dispatch/README.md ("Writing check"), and GET /api/v1/writing-check/counts counts one UTC
-- day of it.
--
-- The server inserts the row outside the write's own transaction, after the decision and before
-- the answer, so a failed insert is logged and never fails the write, and a write that rolls back
-- keeps its row. A daily sweep deletes the rows older than 90 days, on created_at's index.
--
-- token_id is the personal token row the write was made with, where there is one: the day's
-- exemptions are read by it, so a daemon-named write made with a person's token is counted under
-- that token. rules and fields are the rule ids the write broke and the request fields they
-- stand in, for a refused or a forced write; candidates is how many sentences the code stage
-- gave the reader, and reader_ms how long the reader took, null where it was not called.
--
-- A new table no earlier release names: nothing existing is read or rewritten, and the release
-- before this one neither reads nor writes it, so it rolls (cmd/dispatch/README.md, "Database
-- migrations") and the census answers 0. The identifier is the UTC moment this was written, as
-- `migrationid` requires of every migration past the numbered series.
create table writing_check_events (
  id bigint generated always as identity primary key,
  created_at timestamptz not null default now(),
  route text not null,
  method text not null,
  actor_kind text not null,
  actor_id text not null,
  act_session text,
  token_id text,
  outcome text not null check (outcome in (
    'refused', 'forced', 'exempt_person', 'exempt_daemon', 'exempt_system', 'exempt_unread_upload',
    'clean', 'reader_timeout', 'reader_error', 'reader_malformed', 'reader_budget', 'candidate_cap',
    'model_mismatch', 'no_model'
  )),
  override_value text check (override_value in ('agent', 'generated')),
  rules text[] not null default '{}',
  fields text[] not null default '{}',
  candidates int not null default 0,
  reader_ms int,
  detail text
);

-- The sweep deletes by age and the counts route reads one day, both by created_at. The table is
-- new and empty, so the index is built with it.
create index writing_check_events_created_at_idx on writing_check_events (created_at);
