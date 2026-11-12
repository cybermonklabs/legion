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
-- issue still owes that role, which is exactly the work a stopped tree is waiting for. That row
-- is the start of entry 1, so the fill stamps the number into its payload as well: it is the row
-- that will deliver that work, and a delivery carrying no entry would leave the claim at entry 0
-- under a phases row at entry 1. Where that row's own claim is already at entry 1 — the retry of
-- a phase the role never left — the stamp carries the resend mark beside the number, so the
-- entry the claim holds does not decline the task the row was written to give.
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
-- migrations here are forward-only and no .down.sql file exists — so the way back is these seven
-- statements, to run with the daemon stopped:
--
--   alter table phases add column assignment text not null default '';
--   alter table claims add column serving_assignment text not null default '';
--   alter table pending_task_deliveries add column assignment text not null default '';
--   update outbox set payload = payload - 'entry' - 'resendEntry'
--     where kind = 'supervise' and (payload ? 'entry' or payload ? 'resendEntry');
--   update phases set entry = 0, entry_task = '', entry_raised_at = null;
--   update claims set given_entry = 0, served_entry = 0;
--   update pending_task_deliveries set entry = 0;
--
-- The first three statements are the columns that daemon reads; the fourth is the two payload
-- keys this release writes, taken back out. This release writes entry into a supervise start's
-- payload and resendEntry onto a start that resends an entry (record.SuperviseRequest.Entry and
-- ResendEntry); the release before it knows neither key and decodes an outbox payload strictly
-- (record.DecodeOutboxPayload, DisallowUnknownFields), and its record.Postgres.RoleRuns decodes
-- every supervise row of every issue an admission pass reads. A row carrying either key left in
-- the table therefore fails to decode for ever — the runner retries it on its backoff and every
-- admission pass of its issue fails with it — which is why the recovery is more than the three
-- columns.
--
-- The last three statements are for the roll-forward that follows the rollback rather than for
-- the older daemon, which reads none of the six columns this migration adds. That daemon moves
-- issues on by the token it mints while it runs and leaves entry, entry_task, entry_raised_at,
-- given_entry, served_entry and the delivery's entry exactly as the rollback found them, and
-- this migration does not run a second time when the newer image comes back: its version row
-- stands. Left as they were, those numbers describe work that is over. A role that was behind
-- its entry at the rollback would be sent its entry_task from before the rollback — an old head,
-- an old reason — by the first sweep after the roll-forward (workflow.Engine.SendOwedEntry), and
-- a claim left at served_entry 1 under a phases row the older daemon has since moved through two
-- phases would have the fence weigh numbers that describe neither side's work. Put back to zero,
-- the record the roll-forward reads is level everywhere: a role at entry 0 whose claim is at 0 is
-- owed nothing and is sent nothing (workflow.OwedEntry), the fence compares two zeros and so
-- refuses no completion in flight (internal/api/handoff.go), a worker left with no task is still
-- repaired by the repair that reads the slot and not the entry (admit's resumeSlottedWorkers),
-- and the first transition after the roll-forward raises entry 1 again with the task it composes
-- itself (workflow.Engine.enter).
--
-- assignment is not stripped with them. record.SuperviseRequest.Assignment is kept in this
-- release as a field nothing writes, because a rollback is followed by a roll-forward: the older
-- daemon mints that token again at each role's next start, so rows carrying assignment are
-- written after any strip of the key has run, and this release's decoder would refuse them had
-- the field been deleted. The release after this one may delete the field, and a strip of the
-- key belongs there, beside it.
--
-- What the recovery costs the older daemon is nothing beyond those statements. Its fence compares
-- the two sides only when both name an assignment (internal/api/handoff.go, the stale-assignment
-- fence), and the columns come back empty on every row, so it reads nothing to compare: it
-- accepts the completions in flight rather than refusing them, and mints a fresh token at each
-- role's next start, which stamps both sides in one transaction. The entry columns are read by
-- nothing in that daemon, which is why the recovery may put them back to zero while it runs: the
-- zeroing is owed to the roll-forward alone.
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
-- start marked needsSlot, which is the row a stopped tree is waiting on. The row is matched on
-- the token the role's phases row names, as every statement of the fill is, so this statement and
-- the stamp below pick the same row: an issue can owe two such rows for one role — an older one
-- a lane held when the daemon stopped and the one a later transition wrote beside it — and taken
-- by age alone the task read here would be the work of an entry the role has left while the row
-- stamped with the entry carried the work it is on. Where the issue owes two rows of the token
-- itself the oldest is the one the waiting line would run (created_at, then id), so its task is
-- the one the role is owed. A role whose phases row holds no assignment is at entry 0 and takes
-- no task: nothing is owed under an entry that was never raised.
update phases p set entry_task = owed.task
from (
  select distinct on (o.issue, o.payload->>'role')
    o.issue as issue, o.payload->>'role' as role, coalesce(o.payload->>'task', '') as task
  from outbox o
  join phases ph on ph.issue = o.issue and ph.role = o.payload->>'role'
  where o.kind = 'supervise' and o.payload->>'op' = 'start' and o.payload->>'needsSlot' = 'true'
    and ph.assignment <> '' and o.payload->>'assignment' = ph.assignment
  order by o.issue, o.payload->>'role', o.created_at, o.id
) owed
where p.issue = owed.issue and p.role = owed.role and p.assignment <> '';

-- The start row the issue still owes that role is the start of entry 1, so it carries that
-- number. The payload the release before this one wrote names no entry, and the delivery that row
-- makes would leave the claim's given_entry at 0 under a phases row at entry 1: a role read as
-- behind work its worker already holds, which admission and the daemon's own entry sweep close by
-- sending that same work a second time (workflow.OwedEntry, Engine.SendOwedEntry). The row is
-- matched on the token its payload carries, as every statement of the fill is, so a start row
-- carrying any other token — the work of an entry the role has left — takes no entry and stands
-- as it stood.
update outbox o set payload = jsonb_set(o.payload, '{entry}', '1'::jsonb)
from phases p
where o.kind = 'supervise' and o.payload->>'op' = 'start'
  and p.issue = o.issue and p.role = o.payload->>'role'
  and p.assignment <> '' and o.payload->>'assignment' = p.assignment;

-- A stamped row whose claim the fill above counts as holding that entry is marked a resend, so
-- the claim takes its task rather than declining it. Two starts of the release before this one
-- hand a role the work its phases row already names and so leave such a row: the held phase's
-- retry and the slot repair that goes through it (its own workflow.Engine.restart and
-- ResumeSlottedWorker), each writing a start carrying the token the row names, each decided by
-- its outbox row alone, because that release had no entry to decide it by. Carrying the entry and
-- no mark, that row is decided by the entry instead: a claim the two claim statements put at
-- given_entry 1 declines the task (supervise.Machine.queue, an Entry at or below GivenEntry), the
-- start relaunches the worker with no prompt, workflow.OwedEntry answers false, and the gate and
-- `legion state` read the role as level while nobody is at work on it — the class this change
-- closes, made again by the upgrade. The mark puts those rows back on the rule that release ran
-- them under: a start marked a resend is decided by its row (supervise.Claim.DeliveredRows), so
-- one that already delivered its task stays declined and one that never did gives it.
--
-- It is a statement of its own because the mark belongs on those rows and not on every stamped
-- row. A row whose claim the fill leaves at given_entry 0 must stay decided by the entry, which
-- passes its task on its own, and the row rule would be wrong for it: the release before this one
-- wrote a later transition's start over the start an issue already owed in place, keeping the
-- row's id (record.Postgres.EnqueueStart writes a row of its own since this change), so the
-- record this migration finds holds rows the claim has already queued a task from that carry a
-- payload which never reached the worker. That row is the stopped tree itself, and marked a
-- resend it would be declined for the delivery of a task nobody was given.
--
-- Every row this marks carries a task and a phase, which the mark requires of a payload
-- (record's validateOutboxPayload, applied by DecodeOutboxPayload on every read): the one writer
-- of an assignment onto a supervise payload in that release is its workflow started, which writes
-- every such start through StartOrOwe with the phase's task and needsSlot, and needsSlot requires
-- both of them already.
update outbox o set payload = jsonb_set(o.payload, '{resendEntry}', 'true'::jsonb)
from phases p, claims c
where o.kind = 'supervise' and o.payload->>'op' = 'start'
  and p.issue = o.issue and p.role = o.payload->>'role'
  and p.assignment <> '' and o.payload->>'assignment' = p.assignment
  and c.issue = p.issue and c.role = p.role and c.given_entry > 0;

alter table phases drop column assignment;
alter table claims drop column serving_assignment;
alter table pending_task_deliveries drop column assignment;
