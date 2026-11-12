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
-- migrations here are forward-only and no .down.sql file exists — so the way back is these eleven
-- statements, to run in this order with the daemon stopped:
--
--   alter table phases add column assignment text not null default '';
--   alter table claims add column serving_assignment text not null default '';
--   alter table pending_task_deliveries add column assignment text not null default '';
--   update outbox set payload = payload - 'entry' - 'resendEntry'
--     where kind = 'supervise' and (payload ? 'entry' or payload ? 'resendEntry');
--   alter table phases drop column entry;
--   alter table phases drop column entry_task;
--   alter table phases drop column entry_raised_at;
--   alter table claims drop column given_entry;
--   alter table claims drop column served_entry;
--   alter table pending_task_deliveries drop column entry;
--   delete from schema_version where version = 20261111225552;
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
-- The six drops and the deleted version row are for the roll-forward that follows the rollback
-- rather than for the older daemon, which reads none of the six columns this migration adds.
-- Together they re-arm this migration. store.apply decides each migration on its own row —
-- versionRecorded asks `select exists (select 1 from schema_version where version = $1)` — so
-- deleting one row leaves this one migration unapplied and every other migration recorded and
-- untouched, and the older daemon never asks for this version at all, its embedded set holding no
-- such file. All the missing row costs while that image runs is the number the state read reports
-- (internal/daemon's state source, Store.SchemaVersionTx, the highest row recorded), which then
-- reads the version below this one — which is the truth about the image that is running. When the
-- newer image comes back, Migrate finds no row of 20261111225552 and runs this file again from
-- its first statement. The six columns have to go with the row because those first statements add
-- them and `add column` on a column that already stands fails, so dropping them is what lets the
-- file run a second time.
--
-- What the second run fills from is the record the older daemon kept current while it ran. That
-- daemon moves every issue on by the token it mints at each phase change, writing it onto the
-- role's phases row and onto the start that carries the phase's task, so at the roll-forward the
-- three assignment columns the first statements brought back hold who was handed what, as current
-- as the moment the image was swapped back. The fill reads them exactly as it reads them on a
-- first upgrade, and the shapes it meets are the shapes it is proved on
-- (internal/daemon/upgrade_phase_entry_test.go).
--
-- What that costs is one fill run a second time over a record that has moved on. Every role that
-- release had handed work is numbered entry 1 again, whatever entry it stood at before the
-- rollback, and entry_raised_at reads the moment of the roll-forward rather than of the transition
-- that handed the work out. Nothing reads an entry across the rollback — the numbers of both sides
-- went with the columns, and what a role is owed is read as the difference between its phases row
-- and its claim, both written by this one run (workflow.OwedEntry) — so the renumbering costs no
-- more than the history it does not keep. A completion in flight is accepted exactly as it is on a
-- first upgrade: the fill puts a claim that took the work at served_entry 1 under a row at entry 1,
-- leaves a claim that never got it at 0, and the fence compares the two sides only where both name
-- an entry (internal/api/handoff.go).
--
-- Why the re-arm is better than leaving the six columns standing at zero, which is what this
-- header named before: nothing re-derives a lost task from zeros. A role at entry 0 whose claim is
-- at 0 is owed nothing and is sent nothing — workflow.OwedEntry answers false while given is at or
-- above entry, and the repair that reads the slot sends only what admit's owedEntryRoles selects,
-- which is that same answer — and its entry_task is empty besides, so no pass has anything to send
-- it. The role that needs sending is the one whose task the older daemon lost inside the rollback
-- window: that release writes a later transition's start over the start an issue already owes, in
-- place, so a claim that has already queued a task from that row is left holding an id whose
-- payload never reached its worker, and a start finished without acting leaves no row at all.
-- Against zeros both stand holding a slot with nobody at work on them until a person notices.
-- Re-armed, the fill reads the token that daemon left on the phases row, numbers the role entry 1
-- and takes the task of the start it still owes, and the first sweep after the roll-forward sends
-- the work of entry 1 to every role whose claim is behind it — the phase's resume task where the
-- role owes no start and the record holds no task of that entry (workflow.Engine.SendOwedEntry,
-- ResumePhaseTask), which is the shape a start finished without acting leaves.
--
-- The fourth statement strips the two keys this release adds and leaves assignment where it is.
-- record.SuperviseRequest.Assignment is kept in this release as a field nothing writes, because
-- a rollback is followed by a roll-forward: the older daemon mints that token again at each
-- role's next start, so rows carrying assignment are written after any strip of the key has run,
-- and this release's decoder would refuse them had the field been deleted. The release after
-- this one may delete the field, and a strip of the key belongs there, beside it.
--
-- What the recovery costs the older daemon is nothing beyond those statements. Its fence compares
-- the two sides only when both name an assignment (internal/api/handoff.go, the stale-assignment
-- fence), and the columns come back empty on every row, so it reads nothing to compare: it
-- accepts the completions in flight rather than refusing them, and mints a fresh token at each
-- role's next start, which stamps both sides in one transaction. The entry columns are read by
-- nothing in that daemon, which is why the recovery may drop them while it runs: the drop and the
-- deleted version row are owed to the roll-forward alone.
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

-- The start rows the issue still owes its roles, built once here and read by the three statements
-- below: the task of entry 1 is taken from one of them, and the entry and the resend mark are
-- stamped into the payloads of all of them. One set is what holds those three to one reading of
-- what a role is owed. Written out three times, as they were, the copies drifted — only the task
-- fill asked for needsSlot, and the header's claim that the task taken and the row stamped are the
-- same row was true of the sentence alone.
--
-- It is a temporary table where migration 0044 builds its set in a CTE, because a CTE lives in the
-- statement that names it and these three cannot be one statement: two of them write the same
-- outbox rows, and a second write of a row inside one statement is not seen by it. The table is
-- dropped when this migration's transaction commits, which is the transaction store.apply runs
-- every migration in. The set is a snapshot taken before the three statements run, which is sound
-- because none of them writes a column it is selected on.
--
-- A row belongs to the set when it is a supervise start marked needsSlot carrying the token the
-- role's phases row names. needsSlot is in the set rather than in the one statement that asked for
-- it before, for two reasons: it is what "still owed" means, since such a row is waiting for the
-- issue's admission slot and so is the row a stopped tree is waiting on; and it holds of every
-- start carrying an assignment at all, because the one writer of an assignment onto a supervise
-- payload in the release before this one is its workflow started, every such start goes out
-- through StartOrOwe, and StartOrOwe marks needsSlot itself rather than taking the mark from its
-- caller. So the three statements pick the same rows, which is what the stamp requires. The token
-- is what the set is matched on, and not the role's newest row: an issue can owe two starts for
-- one role — an older one a lane held when the daemon stopped and the one a later transition
-- wrote beside it — and a start carrying a token the phases row no longer names is the work of an
-- entry the role has left. A role whose phases row holds no assignment puts no row in the set: it
-- is at entry 0 and nothing is owed under an entry that was never raised.
create temporary table phase_entry_owed_start on commit drop as
select o.id as row_id, o.issue as issue, p.role as role,
  coalesce(o.payload->>'task', '') as task, o.created_at as created_at
from outbox o
join phases p on p.issue = o.issue and p.role = o.payload->>'role'
where o.kind = 'supervise' and o.payload->>'op' = 'start' and o.payload->>'needsSlot' = 'true'
  and p.assignment <> '' and o.payload->>'assignment' = p.assignment;

-- The work of entry 1 is the task of the start row the issue still owes that role. Where the set
-- holds two rows of one role the oldest is the one the waiting line would run (created_at, then
-- id), so its task is the one the role is owed, and the newer row is the retry of the same entry.
update phases p set entry_task = owed.task
from (
  select distinct on (issue, role) issue, role, task
  from phase_entry_owed_start
  order by issue, role, created_at, row_id
) owed
where p.issue = owed.issue and p.role = owed.role;

-- Every start row of the set is a start of entry 1, so each carries that number. The payload the
-- release before this one wrote names no entry, and the delivery such a row makes would leave the
-- claim's given_entry at 0 under a phases row at entry 1: a role read as behind work its worker
-- already holds, which admission and the daemon's own entry sweep close by sending that same work
-- a second time (workflow.OwedEntry, Engine.SendOwedEntry). A start row carrying any other token —
-- the work of an entry the role has left — is no part of the set and stands as it stood.
update outbox o set payload = jsonb_set(o.payload, '{entry}', '1'::jsonb)
from phase_entry_owed_start owed
where o.id = owed.row_id;

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
-- Every row of the set carries a task and a phase, which the mark requires of a payload (record's
-- validateOutboxPayload, applied by DecodeOutboxPayload on every read): the starts that carry an
-- assignment are written through StartOrOwe with the phase's task and needsSlot, and needsSlot
-- requires both of them already.
update outbox o set payload = jsonb_set(o.payload, '{resendEntry}', 'true'::jsonb)
from phase_entry_owed_start owed, claims c
where o.id = owed.row_id and c.issue = owed.issue and c.role = owed.role and c.given_entry > 0;

alter table phases drop column assignment;
alter table claims drop column serving_assignment;
alter table pending_task_deliveries drop column assignment;
