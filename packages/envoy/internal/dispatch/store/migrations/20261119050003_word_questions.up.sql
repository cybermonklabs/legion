-- 20261119050003_word_questions.up.sql — the records of the words people ask agents about.
--
-- INFRA-3231's R8: a job of the server (internal/dispatch/wordrecords) reads the texts people
-- write from the event log, after each write has committed, and asks a model whether the text asks
-- what a word of an agent's text means, says the person cannot understand the text, or complains
-- about its words (the selection prompt, wordrecords/select.md). A person's Define use is recorded
-- the same way (store.RecordDefineUse). The reviewer, Rewrite and Define read a reader's recorded
-- words, and GET /api/v1/writing-check/counts counts the records.
--
-- word_questions holds one row for each text a person wrote that the job read, and one for each
-- Define use. A row holds no text: event_id names the event the text is read from, and answers
-- names the agent text it answers ("ask:<id>", "comment:<id>", "message:<id>",
-- "version:<document id>:<number>"), null where the job found none. text_key is what makes a text
-- one row however often it is read: "event:<id>" for most texts, "broadcast:<id>" for every copy
-- of a person's broadcast, "define:<definition id>" for a Define use.
--
-- person is the login of the person who wrote the text, null where the event's credential names
-- none (a text a person typed in a session that signs in with a pod's shared token), which the
-- counts give apart. project is the project of the text's issue or document, null for a text on
-- neither. day is the UTC day the text was written, which the counts group by.
--
-- A row starts pending. The job classifies it after the write's transaction has committed, so no
-- write waits on the model; a classification that fails leaves the row failed, with its attempts
-- and the time of its next attempt (a backoff that doubles), and the job reads it again until a
-- verdict comes. A row with a verdict is done: verdict is the selection prompt's, model the model
-- the provider served, and words the words the person asked about, empty for a verdict that is
-- not about words. about_words is whether the verdict is one of the three about words, which are
-- the word questions the counts and the reader's words read.
--
-- word_records_cursor is how far the job has read the event log: one row, whose event_id is the
-- highest events.id it has read. It starts null: the first pass starts at the first event created
-- at or after DISPATCH_WORD_RECORDS_FROM, which is the backfill. A pass reads only up to the safe
-- horizon (events.Horizon) and holds this row `for update` while it reads a page, as
-- suggestion_outcome_cursor (0154) is held.
--
-- Two new tables no earlier release names: nothing existing is read or rewritten, no foreign key
-- reaches another table (an event a test database truncates must not be held by a record), and
-- the release before this one neither reads nor writes them, so this rolls (cmd/dispatch/README.md,
-- "Database migrations") and the census answers 0. The identifier is a UTC moment of the forward
-- merge of main on 2026-11-19, which put this migration after main's newest, as `migrationid`
-- requires of every migration past the numbered series.
create table word_questions (
  id bigint generated always as identity primary key,
  source text not null check (source in ('event', 'define')),
  text_key text not null unique,
  event_id bigint,
  person text,
  project text,
  written_at timestamptz not null,
  day date not null,
  kind text,
  answers text,
  verdict text check (verdict in (
    'asks_word_meaning', 'cannot_understand', 'jargon_complaint', 'asks_agent_question', 'none'
  )),
  about_words boolean generated always as (
    coalesce(verdict in ('asks_word_meaning', 'cannot_understand', 'jargon_complaint'), false)
  ) stored,
  words text[] not null default '{}',
  model text,
  state text not null default 'pending' check (state in ('pending', 'done', 'failed')),
  attempts int not null default 0,
  next_attempt_at timestamptz not null default now(),
  last_error text,
  created_at timestamptz not null default now(),
  constraint word_questions_event check ((source = 'event') = (event_id is not null)),
  constraint word_questions_verdict check ((state = 'done') = (verdict is not null))
);

-- The job reads the rows whose classification is due, oldest due first; the counts read days;
-- the reader's words read one person's done rows, newest first.
create index word_questions_due on word_questions (next_attempt_at, id) where state <> 'done';
create index word_questions_day on word_questions (day);
create index word_questions_person on word_questions (person, written_at desc) where state = 'done';

create table word_records_cursor (
  singleton boolean primary key default true check (singleton),
  event_id bigint
);
insert into word_records_cursor default values;
