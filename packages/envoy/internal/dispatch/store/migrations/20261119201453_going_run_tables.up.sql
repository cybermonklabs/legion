-- 20261119201453_going_run_tables.up.sql — a run of the command bar is a job of its own: its
-- conversation, its state and lease, and its steps, events, grant, landings and plans.
--
-- A run is a model loop that goes on after the bar closes, in a process of its own that holds no
-- database and no secret (envoy-dispatch agent). The server claims a ready run for that process
-- under a lease, saves each step it reports, runs each read and write it asks for as the run's
-- person, and records how its turn ended. A conversation is a chain of runs, each one a
-- follow-up of the run before it. History keeps 7 days: it lists a person's conversation for 7
-- days after the conversation's newest run, and the sweep deletes a conversation whole, every
-- run of it and every row below, once its newest run is 7 days old (api/going_runs.go,
-- goingRunsKept).
--
-- going_runs gains:
--
-- - conversation_id, the conversation the run belongs to, named by the id of its first run, so
--   the first run of a conversation names its own id.
-- - continues, the run this one follows, null for the first run of a conversation. A run is
--   deleted with the run it follows (on delete cascade), so no row names a run that is gone.
-- - level, the budget the person chose for the run: 'fast', 'standard' or 'deep'.
-- - mode, which of the run's writes wait for the person: 'ask' or 'auto_review', as
--   user_going_settings.mode names them.
-- - state, where the run is: 'queued' (ready for the agent), 'working' (an agent holds its
--   lease), 'waiting' (its plan waits for the person), 'done', 'stopped' or 'failed'; and
--   stop_reason, why it stopped or failed.
-- - shape, the version of the saved shape of the run and its steps. A build claims only a run
--   whose shape is at or below the highest shape it reads (server_processes.going_run_shape), so
--   a run a newer build saved waits for a build that reads it.
-- - lease_token, lease_owner and lease_ends_at, the agent's lease of a working run: a random
--   token each claim makes anew, which each step, call and end of the run carries and the server
--   checks in the transaction that writes it, so an owner that lost its lease writes nothing; the
--   agent instance that holds it; and when it runs out. A working run whose lease ran out is ready
--   again, so the run of an agent that died is claimed again.
-- - budget_used, what the run has spent of its level's budget: its model calls, its working
--   milliseconds and its tokens, all 0 until it spends them.
-- - kills, how many times the agent died while it worked on this run alone.
--
-- going_runs_ready is the claim's read, the runs it can take: every queued run, and every working
-- run, of which the claim takes the one whose lease ran out, oldest first. going_runs_conversation
-- is the sweep's, each conversation's runs by their time, newest first, and going_runs_continues
-- the read of the follow-ups a deleted run takes with it.
--
-- New tables, each of them keyed by its run and deleted with it (on delete cascade), so the
-- sweep's delete of a conversation's runs deletes the rest with them:
--
-- - going_run_steps, the run's steps in order, from which each step of the loop starts afresh:
--   one model call with its reply, or one tool call with its result. kind is which, body the
--   whole step, and note the one-line summary a run's history sends in place of an older step's
--   body, which the step saves when it is written.
-- - going_run_events, the frames the bar shows of the run, in sequence from 1: sequence is the
--   frame's place in the stream, the Last-Event-ID a reader resumes after, name the frame's name
--   and data its payload, as going_events holds an answer's frames.
-- - going_run_grants, the credential the run acts with: who started the run, or approved its plan
--   last, and by what. kind is 'sign_in' (a sign-in cookie), 'iap' (a sign-in through IAP),
--   'token' (a personal agent token) or 'header' (the header identity of a test server or a
--   local deployment); login is the person; generation and expires_at are a sign-in's
--   generation of the person's sign-ins and the cookie's expiry; token_id is the agent_tokens
--   row of a token. The row holds no cookie, no IAP assertion, no token and no hash of one: the
--   server checks what it names against the store at each call the run makes, so a sign-out, an
--   expired sign-in, a revoked token or a lost membership stops the run. A grant of a token goes
--   with its token's row.
-- - going_landings, one row for each request a call of the run sent in-process that committed,
--   written in that request's own transaction, so the row exists exactly when the change does.
--   Its key is the call's idempotency key: the run, phase ('do' for the call of a run, 'undo' for
--   its Undo), step, call, and request, the number of the request within the call, since one
--   call can send more than one. method, path and body_digest (the hex SHA-256 of its body) are
--   the request; answer is its status and body, which a second try of the call answers from
--   without sending it again; recovery is the delivery replay a message's or comment's request
--   needs, null for any other; landed_at is when it committed. A row is never updated: it is
--   written once, in the transaction that landed its request, and deleted only with its run.
-- - going_plans, the plans the run proposed, numbered within the run, each with its state
--   ('waiting', 'approved', 'rejected', 'running' or 'done') and who approved it and when; and
--   going_plan_rows, each plan's changes, numbered within it: change is the tool's name and its
--   arguments, needs the numbers of the rows of its plan it needs, state where the row is
--   ('waiting', 'approved', 'unticked', 'running', 'done', 'failed' or 'skipped'), approval the
--   person's tick and any value they edited, and result what its write answered. A plan's rows go
--   with their plan.
--
-- Each column going_runs gains is null, or its constant default, on every row stored before it and
-- on every row an insert that does not name it writes, and each check admits null, so every row
-- stored and every insert written before this migration passes, and the ALTER rewrites no row: it
-- holds going_runs ACCESS EXCLUSIVE for its catalog writes and the scan its checks make. A row
-- with no state is no run, and no lease takes it. The six tables are new and empty, so their
-- indexes are built with them, and their foreign keys hold agent_tokens in SHARE ROW EXCLUSIVE
-- while they are created. The release before this one neither reads nor writes any of it, so the
-- two roll side by side (cmd/dispatch/README.md, "Database migrations"). The identifier is the
-- UTC moment this was written, as `migrationid` requires of every migration past the numbered
-- series.
alter table going_runs
  add column conversation_id uuid,
  add column continues uuid references going_runs (id) on delete cascade,
  add column level text check (level in ('fast', 'standard', 'deep')),
  add column mode text check (mode in ('ask', 'auto_review')),
  add column state text check (state in ('queued', 'working', 'waiting', 'done', 'stopped', 'failed')),
  add column stop_reason text,
  add column shape integer,
  add column lease_token uuid,
  add column lease_owner text,
  add column lease_ends_at timestamptz,
  add column budget_used jsonb not null default '{"calls": 0, "working_ms": 0, "tokens": 0}',
  add column kills integer not null default 0;

create index going_runs_ready on going_runs (created_at) where state in ('queued', 'working');

create index going_runs_conversation on going_runs (conversation_id, created_at desc);

create index going_runs_continues on going_runs (continues);

create table going_run_steps (
  run_id uuid not null references going_runs (id) on delete cascade,
  step integer not null,
  kind text not null,
  body jsonb not null,
  note text not null,
  created_at timestamptz not null default now(),
  primary key (run_id, step)
);

create table going_run_events (
  run_id uuid not null references going_runs (id) on delete cascade,
  sequence bigint not null check (sequence > 0),
  name text not null,
  data jsonb not null,
  created_at timestamptz not null default now(),
  primary key (run_id, sequence)
);

create table going_run_grants (
  run_id uuid primary key references going_runs (id) on delete cascade,
  kind text not null check (kind in ('sign_in', 'iap', 'token', 'header')),
  login text not null,
  generation bigint,
  expires_at timestamptz,
  token_id uuid references agent_tokens (id) on delete cascade,
  granted_at timestamptz not null default now()
);

create table going_landings (
  run_id uuid not null references going_runs (id) on delete cascade,
  phase text not null check (phase in ('do', 'undo')),
  step integer not null,
  call integer not null,
  request integer not null,
  method text not null,
  path text not null,
  body_digest text not null,
  answer jsonb not null,
  recovery jsonb,
  landed_at timestamptz not null default now(),
  primary key (run_id, phase, step, call, request)
);

create table going_plans (
  run_id uuid not null references going_runs (id) on delete cascade,
  plan integer not null,
  state text not null check (state in ('waiting', 'approved', 'rejected', 'running', 'done')),
  created_at timestamptz not null default now(),
  approved_at timestamptz,
  approved_by jsonb,
  primary key (run_id, plan)
);

create table going_plan_rows (
  run_id uuid not null,
  plan integer not null,
  row integer not null,
  change jsonb not null,
  needs integer[] not null default '{}',
  state text not null check (state in ('waiting', 'approved', 'unticked', 'running', 'done', 'failed', 'skipped')),
  approval jsonb,
  result jsonb,
  primary key (run_id, plan, row),
  foreign key (run_id, plan) references going_plans (run_id, plan) on delete cascade
);
