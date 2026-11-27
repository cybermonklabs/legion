-- 20261117132340_inbox_comparison_runs.up.sql — one row per run of `envoy-dispatch inbox-compare`.
--
-- The spec's R8 and R10: the comparison asks the three reads the one read replaces
-- (GET /api/v1/inbox?assignee=me, GET /api/v1/me/replies and a person's own conversations of GET
-- /api/v1/agents/{session_id}/messages) on the build of the release before, and the one read (GET
-- /api/v1/me/inbox) on this build, both over one database, for every person, and keeps what each
-- run found here. The gate reads these rows and nothing else (GET /api/v1/inbox/comparison answers
-- the newest one's counts), so a thin corpus stays readable afterwards rather than inferred.
--
-- The command inserts one row per run and nothing updates or deletes one: a clean run leaves an
-- earlier mismatch's row as it was written. internal/dispatch/inboxcompare is the one writer.
--
--   difference_count  the rows an old read answered and the one read did not, every person and
--                     every arm together; differences holds exactly that many.
--   people_checked    every person the database holds, the ones the run did not reach among them.
--   people_uncovered  the people the run did not reach: one `--exclude` named, or one an answer of
--                     either server left unread.
--   shapes            the shapes of the corpus the gate requires, each with the rows the run found
--                     of it, a shape of none included, so a corpus that lacked one says so.
--   differences       one object a difference: the person, the arm (the old read), the kind it is
--                     counted under, the old read's row and the node that row stands for.
--
-- The comparison runs in the branch alone and never against a deployment (it signs in through dev
-- sign-in only), and the commit that moves the callers off the three reads drops this table with
-- the command and its page, so no release holds it.
--
-- Rolling deploy: a new table, which the release before names nowhere.
create table inbox_comparison_runs (
  id bigint generated always as identity primary key,
  ran_at timestamptz not null default now(),
  difference_count integer not null check (difference_count >= 0),
  people_checked integer not null check (people_checked >= 0),
  people_uncovered integer not null check (people_uncovered between 0 and people_checked),
  shapes jsonb not null check (jsonb_typeof(shapes) = 'object'),
  differences jsonb not null check (jsonb_typeof(differences) = 'array'),
  check (jsonb_array_length(differences) = difference_count)
);
