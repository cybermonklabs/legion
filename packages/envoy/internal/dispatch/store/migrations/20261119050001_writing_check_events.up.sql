-- 20261119050001_writing_check_events.up.sql — one row for each write the writing check decided.
--
-- INFRA-3231: the server reads the prose of an agent's write before it stores it
-- (api/writing_check.go), and the write goes on whatever the check finds. This table records what
-- the check decided for each write it saw: a sentence that breaks a rule (findings), written
-- clean, or not read at all because the writer is a person, a project's daemon or Dispatch
-- itself, because a plugin generated the text (exempt_generated), because a person typed the
-- command that wrote it in a session (exempt_person_typed), or because the upload is a file the
-- check does not read. It also records what the reviewer did with an agent's text: held it
-- (held), let it post as the review found nothing (passed), had the writer edit it (edited) or
-- post it as written over the findings (posted_as_written), posted its own rewrite
-- (rewrite_posted), posted it with no review (posted_unreviewed), or reviewed it after it posted
-- (reviewed_after, its rules the findings); and each Rewrite a person asks for
-- (rewrite_requested). A person reads it with the SQL in
-- cmd/dispatch/README.md ("Writing check"), and GET /api/v1/writing-check/counts counts it by day,
-- and by day, project and reader.
--
-- The server inserts the row outside the write's own transaction, after the decision and before
-- the answer, so a failed insert is logged and never fails the write, and a write that rolls back
-- keeps its row. A daily sweep deletes a row the counts' rows of each day, project and reader
-- read once it is older than 366 days, the longest range the counts route reads, and every other
-- row once it is older than 90 days (store.SweepWritingCheckEvents).
--
-- token_id is the personal token row the write was made with, where there is one: the day's
-- exemptions are read by it, so a daemon-named write made with a person's token is counted under
-- that token. rules and fields are the rule ids the write broke and the request fields they
-- stand in, for a write with findings. project is the project of the text's issue or document,
-- null where the writer did not record it. write_id is the id of the write the row decides, where
-- more than one row records one write: a held text's id on each row of the held text, and an id
-- the server makes for the write on the check's row of it and on the reviewer's row of its review
-- after the post, so the counts count its text once; null on a row that is its write's only one.
--
-- A new table no earlier release names: nothing existing is read or rewritten, and the release
-- before this one neither reads nor writes it, so it rolls (cmd/dispatch/README.md, "Database
-- migrations") and the census answers 0. The identifier is a UTC moment of the forward merge of
-- main on 2026-11-19, which put this migration after main's newest, as `migrationid` requires of
-- every migration past the numbered series.
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
    'findings', 'exempt_person', 'exempt_daemon', 'exempt_system', 'exempt_unread_upload',
    'exempt_generated', 'exempt_person_typed', 'clean', 'held', 'passed', 'edited',
    'posted_as_written', 'rewrite_posted', 'posted_unreviewed', 'reviewed_after', 'rewrite_requested'
  )),
  rules text[] not null default '{}',
  fields text[] not null default '{}',
  detail text,
  project text,
  write_id text
);

-- The table is new and empty, so each index is built with it. The counts route reads days by
-- created_at, and so does the sweep's delete of every row older than 366 days, which reads in that
-- index the rows it deletes alone. Its delete of the rows older than 90 days that the counts do
-- not read reads the partial index of those rows, so its daily pass reads none of the rows it
-- keeps; the index's predicate is store.countedRow negated, as that delete writes it, each outcome
-- a literal, so every plan of the delete can use it. The counts read every row of a write that has
-- a row in their range by write_id, to give its text to one day.
create index writing_check_events_created_at_idx on writing_check_events (created_at);
create index writing_check_events_uncounted_idx on writing_check_events (created_at)
  where not (outcome in ('rewrite_requested', 'held', 'rewrite_posted', 'passed', 'posted_unreviewed') or (outcome in ('findings', 'posted_as_written') or (outcome = 'reviewed_after' and cardinality(rules) > 0)));
create index writing_check_events_write_id_idx on writing_check_events (write_id) where write_id is not null;
