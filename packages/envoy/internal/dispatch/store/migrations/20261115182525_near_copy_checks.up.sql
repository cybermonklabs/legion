-- 20261115182525_near_copy_checks.up.sql
-- The near-copy gate answers every issue create it covers (api's runNearCopyGate), and until this
-- table only a create forced past a refusal left a record of it, the `forced` record on its
-- issue.created event. A refused create wrote nothing anywhere, and no create kept the scores the
-- gate read, so how often the live gate refuses a create that is no copy, or lets a copy in,
-- could not be counted on the creates it actually scored. One row here is one answer.
--
-- Written by the create itself once the gate has answered. A refusal is answered only once its row
-- is known to be written, on a context of its own and bounded: otherwise it refuses nothing, and
-- the create goes in unchecked (not_made, unrecorded), so every 409 the gate gives has its row
-- here. Such a create writes its row in its own transaction, under the id the refusal used, so the
-- row replaces a refused row whose insert landed unseen and commits with the create or not at
-- all. Every other create's row is written after its commit, bounded, and tried once more in the
-- background when a timeout or a lock stopped it; one that still cannot be written is logged and
-- never fails the create, whose issue.created event still records that it went in.
-- GET /api/v1/near-copy-checks reads one project's rows over a span of time, with no spec, and
-- GET /api/v1/near-copy-checks/{id} reads one row with its spec.
create table near_copy_checks (
  id uuid primary key default gen_random_uuid(),
  created_at timestamptz not null default now(),
  project_key text not null references projects(key) on delete cascade,
  actor_kind text not null,
  actor_id text not null,
  -- refused: the gate refused it and nothing was written (every 409 POSSIBLE_DUPLICATE has this
  -- row, written before the answer); forced: went in past a refusal
  -- with `force`; nudged: went in, its closest open issue named as a likely duplicate; passed:
  -- went in with nothing named, forced or not; not_made: the gate made no check that could stop
  -- the create, and it went in unchecked.
  outcome text not null check (outcome in ('refused', 'forced', 'nudged', 'passed', 'not_made')),
  -- Why no check was made: bound, the read ran out of its bound or its request went away; failed,
  -- the read itself failed; unrecorded, the read found a near copy but the row of its refusal
  -- could not be written, so it refused nothing.
  reason text check (reason in ('bound', 'failed', 'unrecorded')),
  check ((outcome = 'not_made') = (reason is not null)),
  -- Whether the create asked for `force`.
  force boolean not null,
  title text not null,
  -- The spec as the create sent it, kept for a refused create alone, which has no issue to hold
  -- it; empty when it sent none.
  spec text,
  check ((outcome = 'refused') = (spec is not null)),
  -- The sha256 of that spec, in hex, on every row.
  spec_sha256 text not null,
  parent_key text,
  -- The issue the create wrote, null for a refused create. No foreign key: the record of what the
  -- gate answered outlives whatever happens to the issue.
  issue_key text,
  check ((outcome = 'refused') = (issue_key is null)),
  -- The candidates the gate read at or above the nudge bar, at most five, best score first, each
  -- {key, title, score}: what a 409 named and what it read below the refusal bar.
  candidates jsonb not null default '[]',
  -- The two bars the scores were read against, and the legion commit the server was built from
  -- (null for a build nothing stamped).
  refuse_score double precision not null,
  nudge_score double precision not null,
  build_commit text
);

-- The route reads one project's rows over a span of time, oldest first.
create index near_copy_checks_project_created on near_copy_checks (project_key, created_at, id);
