-- 20261119050004_held_texts.up.sql — the texts the reviewer holds before any person sees them.
--
-- INFRA-3231's R5: with DISPATCH_REVIEWER_<KIND>=hold, an ask, a new issue, a comment or a message
-- an agent writes to a person is kept here, and nowhere else, until the reviewer has read it and
-- the writer session has acted on what it found (api/reviewer.go). Nothing of a held text is in
-- asks, issues, comments, messages, events, the outbox, the reference graph or search until it
-- posts, so no person's read, count, feed or notice can show it. The post runs the kind's own
-- create path in one transaction with the stored request, the recorded actor and the id reserved
-- here, so it writes the rows and the event the same write writes with the reviewer off.
--
-- id is the reserved id: an issue's key, whose number the hold took from its project's sequence,
-- else the uuid the ask, the comment or the message is created under. request is the decoded
-- write the post runs, the writer's edits applied (PATCH /api/v1/held/{id}); original is the text
-- as the writer first wrote it, which a rewrite that posts keeps as the original. An agent's reply
-- to a delivery it got, and any other write that names a held text (a document, a result, a
-- handoff, a claim or an update of a held issue; kind request), keep their request whole, with
-- the caller that made it, and post by running it again through the server's own route. actor is
-- the recorded actor, writer the session that wrote it, reader the person who reads it, project
-- and issue_key where it stands.
--
-- state is reviewing (the reviewer reads it), awaiting_writer (the review found problems and the
-- writer session has until deadline to edit or post it), posting (decided: outcome says what
-- posts), posted (posted_at is the post's time, which the posted row carries as its creation
-- time), or failed (the post was refused, detail says why). revision counts the writer's edits, so
-- a review of an older wording is discarded. claimed_at is when the review or the post now under
-- way took the row; a row a stopped process left in reviewing or posting is taken again by the
-- reviewer's sweep once that claim is stale. findings, rewrite and not_applied are the review's
-- (reviewer.Reviewed), reason the writer's reason for posting as written, and writer_answer the
-- writer's reply to the frame Dispatch sent it.
--
-- depends_on is the held text a later write named as its parent or as the text it answers; such a
-- write is held beside it and posts right after it, in seq order. written_at is the time of the
-- write. person_typed marks a write a person typed in a session (Dispatch-Person-Typed: 1,
-- Decision 26 of INFRA-3231's plan) that waits on a held parent: it is never reviewed, and its
-- post marks its event person_typed.
--
-- A decision_block is a decision block an agent wrote in a document with
-- DISPATCH_REVIEWER_DECISION_BLOCKS=hold (api/decision_blocks.go): its ask row exists and its
-- document shows it, so the row under the ask's id holds only its review, its writer's window and
-- what posts, and its post ends the hold (asks.held_until), applying the reviewer's rewrite as a
-- new version of its document where the outcome is rewrite_posted.
--
-- A new table no earlier release names: nothing existing is read or rewritten, and the release
-- before this one neither reads nor writes it, so this rolls (cmd/dispatch/README.md, "Database
-- migrations") and the census answers 0. The identifier is a UTC moment of the forward merge of
-- main on 2026-11-19, which put this migration after main's newest, as `migrationid` requires of
-- every migration past the numbered series.
create table held_texts (
  id text primary key,
  kind text not null constraint held_texts_kind
    check (kind in ('ask', 'issue', 'comment', 'message', 'request', 'decision_block')),
  route text not null,
  request jsonb not null,
  original jsonb not null,
  actor jsonb not null,
  writer text,
  reader text,
  project text,
  issue_key text,
  state text not null check (state in ('reviewing', 'awaiting_writer', 'posting', 'posted', 'failed')),
  revision int not null default 1,
  reviewed boolean not null default true,
  findings jsonb not null default '[]',
  rewrite jsonb,
  not_applied text,
  outcome text check (outcome in (
    'passed', 'posted_as_written', 'rewrite_posted', 'posted_unreviewed', 'not_reviewed'
  )),
  reason text,
  detail text,
  writer_answer text,
  deadline timestamptz,
  claimed_at timestamptz,
  depends_on text references held_texts (id),
  seq bigint generated always as identity,
  written_at timestamptz not null default now(),
  person_typed boolean not null default false,
  posted_at timestamptz,
  updated_at timestamptz not null default now(),
  constraint held_texts_deadline check (state <> 'awaiting_writer' or deadline is not null),
  constraint held_texts_posted check ((state = 'posted') = (posted_at is not null)),
  constraint held_texts_outcome check (state not in ('posting', 'posted') or outcome is not null)
);

-- The reviewer's sweep reads the rows still open; a held text's dependents are read in order.
create index held_texts_open on held_texts (updated_at) where state in ('reviewing', 'awaiting_writer', 'posting');
create index held_texts_depends_on on held_texts (depends_on, seq) where depends_on is not null;
