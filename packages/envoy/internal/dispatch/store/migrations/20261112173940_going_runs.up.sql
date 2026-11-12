-- 20261112173940_going_runs.up.sql — the record of one answered pause of the command bar.
--
-- A going request's rows are swept an hour after it is asked (going_requests,
-- 20261105152945, contracts.GoingRequestTTL), and a person undoes a turn of the bar from any page
-- and after a reload, days later. So what one run found, decided, proposed and wrote is a row of
-- its own, kept for thirty days (api/going_runs.go, goingRunsKept), one row per run:
--
-- - id is the going request's own id: the row is written when the run starts, under the id the
--   POST answered, and it outlives that request's sweep. No foreign key, for that reason.
-- - author_login is the asking person's canonical login (model.CanonicalLogin), the one key
--   GET /api/v1/me/going-runs reads by. Nobody else reads a row: the route takes no login.
-- - words is the question, context the bar's context chip (page path, project, issue, block).
-- - hits, decision and proposal are what the run found, what the decision model chose and what
--   the answer offered, each null until the run writes it.
-- - writes and undos are JSON arrays the store appends to one element at a time, in one
--   statement each, so two appends to one run never lose either (api/going_runs.go,
--   goingRunWrite and goingRunUndo are their shapes).
-- - issues is every issue key a hit or a write of the run touched, which the store adds as it
--   writes the hits and appends each write; the route's ?issue= filter reads it through the GIN
--   index below.
-- - structured_output is how the answer call's structured output went, null until it is known.
-- - finished_at is when the run ended; null reads as unfinished, an abandoned run included, which
--   keeps whatever it stored.
--
-- going_runs_author_created is the route's own read, a person's runs newest first.
--
-- A new table no earlier release names: nothing existing is read or rewritten, and the release
-- before this one neither reads nor writes it, so it rolls (cmd/dispatch/README.md, "Database
-- migrations") and the census answers 0. It is new and empty, so its indexes are built with it.
-- The identifier is the UTC moment this was written, as `migrationid` requires of every
-- migration past the numbered series.
create table going_runs (
  id uuid primary key,
  author_login text not null,
  words text not null,
  context jsonb,
  hits jsonb,
  decision jsonb,
  proposal jsonb,
  writes jsonb not null default '[]',
  undos jsonb not null default '[]',
  issues text[] not null default '{}',
  structured_output text,
  created_at timestamptz not null default now(),
  finished_at timestamptz
);

create index going_runs_author_created on going_runs (author_login, created_at desc);

create index going_runs_issues on going_runs using gin (issues);
