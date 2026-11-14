-- 20261114044457_claim_waits.up.sql — the waits a phase worker declares, one standing wait per
-- claim, each kept with its end once it has one (dispatch://INFRA-2040 R2, R3, R4).
--
-- A row is one declaration: the claim that made it (project, claim_token) on its issue and role,
-- the issue's generation and phase and the role's phases row entry it was declared at, the entry
-- of the task the claim had taken (taken_entry), the session it was declared in, what the worker
-- waits on (waiting_on) and when the wait's time comes (wake_at). id is minted by the daemon. A
-- standing wait has no end; an ended one carries when it ended, its cause and the fact that
-- ended it (ended_at, end_cause, ended_by), and is never written again.
--
-- The wait is a table of its own, not a column of claims: putClaim writes every column of
-- claims a claim's machine holds in memory, and clears the activity columns on every write that
-- moves the launch generation, so a wait kept there would be put back from memory or lost at a
-- relaunch. Only record's own statements write this table, each in the transaction of the fact
-- that declares or ends a wait (record.Postgres.DeclareWait, EndWait), and keeping the ended
-- rows keeps the count of how each wait ended.
--
-- claim_waits_standing is what holds one standing wait per claim: a second row of the same claim
-- with no end is refused, so a declaration ends the standing row (cause replaced) before it
-- inserts its own, and the claim's reads (readRoleClaims) find at most one.
--
-- Rolling the image back needs no restore: the release before this one names the table in no
-- statement, so it reads and writes around it, and a daemon rolled forward again finds the rows
-- as they stood.
--
-- There is no down file: migrations here are forward-only.
create table claim_waits (
  id text primary key,
  project text not null,
  claim_token text not null,
  issue text not null,
  role text not null,
  generation bigint not null,
  phase text not null,
  phase_entry bigint not null,
  taken_entry bigint not null,
  session text not null,
  waiting_on text not null check (length(btrim(waiting_on)) > 0),
  wake_at timestamptz not null,
  declared_at timestamptz not null,
  ended_at timestamptz,
  end_cause text,
  ended_by text);
create unique index claim_waits_standing on claim_waits (project, claim_token) where ended_at is null;
