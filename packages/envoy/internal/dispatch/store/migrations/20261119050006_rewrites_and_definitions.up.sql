-- 20261119050006_rewrites_and_definitions.up.sql — a person's Rewrite of an ask, and a person's
-- Define of words (INFRA-3231's R7 and Decisions 23 and 24 of its plan, INFRA-3552's R1 to R4).
--
-- ask_rewrites holds the latest Rewrite a person asked for of each ask (api/ask_rewrite.go). Its
-- row is the one rewrite of the ask that runs at a time: a request while a fresh run is
-- rewriting starts none and is answered that run's state, and a run finishes only its own row
-- (run). state is rewriting while the run works, rewritten once its ask.edited is written, or
-- failed with reason, which the card shows as the failed rewrite's tooltip. asked_by is the
-- person's actor, original the wording the run read ({body, options, multiple}), and
-- question_message the message that holds the side question Dispatch sent the asking session
-- (INFRA-3552's R3: kept as a message to that session, on its own conversation, and linked to the
-- ask), null where none was sent. calls holds each model call the run made, in the shape a
-- definition's calls have ({purpose, deployment, served, input_tokens, output_tokens,
-- cache_write_tokens, cache_read_tokens, fault}), written once its model call ends; [] for a run
-- that made none. The rewritten wording's own record is the ask.edited event, whose previous
-- wording is the original; a run a stopped process left rewriting reads as failed once its time
-- bound has passed, and the next request starts a new run.
--
-- definitions holds each definition a person asked for (api/definitions.go): the words they
-- selected, where they were (an ask, or a document at a version), the parts of Dispatch's record
-- it read (inputs), the definitions read from the record, and the asking agent's own answer to
-- the side question Dispatch sent it, kept here alone: no message row and no event is written for
-- it, so no other person reads it. failure says why the model gave no definition, "" for none.
-- writer is the asking session the side question went to, null for none; agent_question the
-- question, agent_answer its answer and agent_answered_at when it came.
--
-- Two new tables no earlier release names: nothing existing is read or rewritten, and the release
-- before this one neither reads nor writes them, so this rolls (cmd/dispatch/README.md, "Database
-- migrations") and the census answers 0. The identifier is a UTC moment of the forward merge of
-- main on 2026-11-19, which put this migration after main's newest, as `migrationid` requires of
-- every migration past the numbered series.
create table ask_rewrites (
  ask_id uuid primary key references asks (id) on delete cascade,
  run uuid not null,
  state text not null check (state in ('rewriting', 'rewritten', 'failed')),
  asked_by jsonb not null,
  original jsonb not null,
  question_message uuid,
  reason text,
  calls jsonb not null default '[]',
  started_at timestamptz not null default now(),
  finished_at timestamptz,
  constraint ask_rewrites_finished check ((state = 'rewriting') = (finished_at is null))
);

create table definitions (
  id uuid primary key,
  person text not null,
  ask_id uuid references asks (id) on delete cascade,
  artifact_id uuid references artifacts (id) on delete cascade,
  version int,
  project text,
  words text[] not null,
  inputs text[] not null default '{}',
  definitions jsonb not null default '[]',
  failure text,
  calls jsonb not null default '[]',
  writer text,
  agent_question text,
  agent_answer text,
  agent_answered_at timestamptz,
  created_at timestamptz not null default now(),
  constraint definitions_one_subject check ((ask_id is null) <> (artifact_id is null))
);

-- GET /api/v1/me/definitions reads one person's definitions, the newest first.
create index definitions_person on definitions (person, created_at desc);
-- An ask's delete and a document's removal cascade to the definitions that name them, inside the
-- delete's own transaction, so each column a delete cascades by has an index of the rows that
-- name one, as asks_artifact_id and comments_ask_id are; the table is new, so they cost nothing
-- to build.
create index definitions_ask_id on definitions (ask_id) where ask_id is not null;
create index definitions_artifact_id on definitions (artifact_id) where artifact_id is not null;
