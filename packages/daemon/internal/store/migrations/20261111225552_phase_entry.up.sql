-- 20261111225552_phase_entry.up.sql — the phase entry: the number of the work a role is owed, the
-- work itself, and the two numbers that say what each claim was given and what its worker took
-- (INFRA-2800).
--
-- What stands before this migration is one random token (0067's assignment), minted at each phase
-- change onto the role's phases row and carried on the single outbox start row that holds the
-- phase's task. That row is at once the task, the issue's place in the admission line and the
-- receipt that the task was given, and where those three meanings disagreed the tree stopped: the
-- row's payload replaced in place after its earlier payload had already reached the claim left a
-- phase whose work nobody held, with no error and no row behind it, and every completion the
-- worker reported refused for an assignment it was never given.
--
-- The six columns are that statement, held where each fact belongs. phases.entry numbers the
-- entry the role's current phase is on, phases.entry_task is the work of that entry, and
-- phases.entry_raised_at is when the transition raised it. claims.given_entry is the entry of the
-- last task the claim was queued and claims.served_entry the entry of the last task its worker
-- confirmed, so a claim behind its own phases row is a difference the daemon can see and close by
-- sending that entry's task again. pending_task_deliveries.entry carries the entry from the start
-- row to the claim, as the delivery's assignment carried the token.
--
-- The fill puts every role the release before this one had handed work into the new model at
-- entry 1, so the first entry this release raises is 2 and no role is swept for work it is
-- already doing. A phases row holding an assignment is a role that was handed work, so it is at
-- entry 1; a claim whose serving_assignment is that same token took that work, so it was given it
-- and served it; a delivery carrying that token is that work in flight, so the claim was given it
-- even where nothing is served yet; and the task of entry 1 is the task of the start row the
-- issue still owes that role, which is exactly the work a stopped tree is waiting for.
--
-- Every statement of the fill excludes the empty assignment. All three assignment columns are
-- text not null default '', so an equality alone holds between two empty values: a claim that was
-- given nothing, beside a phases row no start ever stamped, would read as a match and take
-- given_entry 1 while its phases row stays at entry 0. That is a claim ahead of the work its role
-- is owed — the inversion of the stopped shape this change exists to repair, and one no sweep
-- would ever close, since the difference it reads runs the other way.
--
-- Once this migration has run, the daemon of the release before it cannot read the claims table
-- at all: its own claim read names serving_assignment (packages/daemon/internal/store/claims.go,
-- claimSelect), so every claim read fails and the supervisor loads nothing. Rolling the image
-- back therefore needs the three columns back first. This directory keeps no down migrations —
-- migrations here are forward-only and no .down.sql file exists — so the way back is these three
-- statements, to run with the daemon stopped:
--
--   alter table phases add column assignment text not null default '';
--   alter table claims add column serving_assignment text not null default '';
--   alter table pending_task_deliveries add column assignment text not null default '';
--
-- That recovery costs nothing beyond the statements. The older daemon's fence compares the two
-- sides only when both name an assignment (internal/api/handoff.go, the stale-assignment fence),
-- and the columns come back empty on every row, so it reads nothing to compare: it accepts the
-- completions in flight rather than refusing them, and mints a fresh token at each role's next
-- start, which stamps both sides in one transaction. The entry columns are left standing by that
-- recovery and are read by nothing in the older daemon.
alter table phases add column entry bigint not null default 0;
alter table phases add column entry_task text not null default '';
alter table phases add column entry_raised_at timestamptz;
alter table claims add column given_entry bigint not null default 0;
alter table claims add column served_entry bigint not null default 0;
alter table pending_task_deliveries add column entry bigint not null default 0;

-- A role the issue's current phase has handed work is at entry 1, raised at the moment this
-- migration runs, which is the only time the upgrade can honestly claim for an entry whose own
-- transition recorded none.
update phases set entry = 1, entry_raised_at = now() where assignment <> '';

-- A claim whose worker took that role's work was given it and served it. The claims table carries
-- the issue and the role of its claim (claimSelect reads both), so the join reads them rather
-- than parsing the token.
update claims c set given_entry = 1, served_entry = 1
from phases p
where p.issue = c.issue and p.role = c.role and p.assignment <> '' and c.serving_assignment = p.assignment;

-- A claim holding that work in flight was given it, whatever its worker has confirmed: the
-- delivery carries the token the role's phases row names. A claim the statement above already
-- counted is left alone, so a served entry is never written back over.
update claims c set given_entry = 1
from pending_task_deliveries d, phases p
where d.claim_token = c.token and p.issue = c.issue and p.role = c.role
  and p.assignment <> '' and d.assignment = p.assignment and c.given_entry = 0;

-- The delivery in flight is of that same entry, so the retirement that records what its worker
-- took has an entry to record.
update pending_task_deliveries d set entry = 1
from claims c, phases p
where c.token = d.claim_token and p.issue = c.issue and p.role = c.role
  and p.assignment <> '' and d.assignment = p.assignment;

-- The work of entry 1 is the task of the start row the issue still owes that role: a supervise
-- start marked needsSlot, which is the row a stopped tree is waiting on. Where an issue owes two
-- such rows for one role the oldest is the one the waiting line would run (created_at, then id),
-- so its task is the one the role is owed. A role whose phases row holds no assignment is at
-- entry 0 and takes no task: nothing is owed under an entry that was never raised.
update phases p set entry_task = owed.task
from (
  select distinct on (o.issue, o.payload->>'role')
    o.issue as issue, o.payload->>'role' as role, coalesce(o.payload->>'task', '') as task
  from outbox o
  where o.kind = 'supervise' and o.payload->>'op' = 'start' and o.payload->>'needsSlot' = 'true'
  order by o.issue, o.payload->>'role', o.created_at, o.id
) owed
where p.issue = owed.issue and p.role = owed.role and p.assignment <> '';

alter table phases drop column assignment;
alter table claims drop column serving_assignment;
alter table pending_task_deliveries drop column assignment;
