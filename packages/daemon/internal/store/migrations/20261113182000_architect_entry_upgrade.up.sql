-- 20261113182000_architect_entry_upgrade.up.sql — the entry every root architect of the release
-- before this one is settled at, so this release starts nothing that is already running
-- (INFRA-2614).
--
-- This release sends each role of a slotted issue the work of the entry its phases row names, and
-- a root's own architect is now one of those roles (admit's resumeSlottedWorkers,
-- workflow.ArchitectAtWork). The release before it raised no entry for an architect and wrote it
-- no phases row at all. Without this fill the first pass after the upgrade would read every root
-- architect as owed an entry it was never given and send each one a first task, so an architect
-- that is writing its tree's spec right now would be told to take the tree up again.
--
-- The record cannot tell that architect from a stuck one. Both stand at admitted with no
-- design-gate rows and hold no delivery, and nothing in the record says whether a task ever
-- reached the claim. The set this leaves unrepaired is measured: INFRA's controller read the
-- daemon state at 2026-11-11T23:45:01Z and found 31 roots at admitted, 16 of them with no design
-- gate recorded. Those 16 are the upper bound, since the state says nothing about whether a task
-- reached a claim. A root admitted, re-admitted or carried over after this release is handed
-- its work by the rule itself, so the fill is for the roots standing when it runs and no other
-- (dispatch://INFRA-2614, R8).
--
-- The fill is the entry 1, on the architect's phases row and on its claim: the row is created
-- where the record holds none, with no task on it and no moment raised, and the claim is recorded
-- as having been queued that entry and as having confirmed it. Every other column takes its
-- default, so the row carries no handoff and no verdict. A claim at the entry its row names is
-- owed nothing (workflow.OwedEntry), and the two numbers with no queued and no confirmed time
-- beside them are how a reader of the state tells a root of the older release from one this
-- release served (api.Issue.EntryQueuedAt), since the fill writes no delivery.
--
-- It fills an architect claim whose issue is its own tree's root and no other. A child's own
-- sub-architect claim is outside the rule the fill is for: no daemon start creates one, and
-- workflow.ArchitectHears answers false for an issue that is not its tree's root, so a row and
-- marks for it would be rows nothing ever reads.
--
-- The marks are the first and the third of the three stall arms (record.ArchitectStallArm),
-- written for each of those roots at the run its record stands at, so this release wakes the
-- project's controller for no root it has not repaired. The first arm reads a task queued to a
-- claim that confirmed nothing, and the third a root standing at admitted with no design-gate step
-- registered, which is what the 16 measured roots are. The second arm is marked for none of them:
-- it reports a claim that is failed or retired, which the record states plainly and no send can
-- cure, so a filled claim that fails or that an operator retires wakes the controller as any other
-- claim does.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction below, run with the daemon
-- stopped and before the one the phase entry's own header carries
-- (20261111225552_phase_entry.up.sql). It is the way back of this migration and of
-- 20261113181500_architect_stalled_notices, whose header carries the same block, and
-- docs/kubernetes.md carries it as the step an operator runs before the phase entry's.
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   delete from schema_version where version = 20261113182000;
--   delete from outbox where kind = 'controller_notice' and payload->>'kind' = 'architect-stalled';
--   drop table if exists architect_stalled_notices;
--   delete from schema_version where version = 20261113181500;
--   commit;
--   SQL
--
-- The run prints BEGIN, DELETE 1, DELETE <n> for the wakes it drops, DROP TABLE, DELETE 1 and
-- COMMIT. A rerun prints BEGIN, DELETE 0, DELETE 0, a NOTICE that the table does not exist, DROP
-- TABLE, DELETE 0 and COMMIT, and changes nothing. The phase entry's block after it leaves the
-- store at the schema of the release before that migration and at that release's own version,
-- 20261112180548, the highest row the two blocks leave standing.
--
-- This block runs first because this migration writes into three of the six columns the phase
-- entry's block drops: the architect's phases.entry and its claim's given_entry and served_entry.
-- That header names the case: a migration recorded above it that builds on those columns keeps
-- its own row through that block's delete and would never run again, so it is reversed by its own
-- way back first.
--
-- The older daemon needs nothing of this migration taken away: it weighs no root architect at
-- all, and what the fill wrote either goes with the columns or is passed over. The two claim
-- numbers go with the columns the phase entry's block drops. The phases rows the insert wrote
-- stand, each without its entry column, and the older daemon passes over them: its start, its
-- retry and its handoff clear skip the architect (workflow's start, restart and clearHandoff), it
-- refuses an architect's completion as it always has, and its state read shows a phases row only
-- through the live claim the row names, where every row this fill writes names none (claim '').
--
-- The delete of this migration's row is owed to the roll-forward. The phase entry's way back
-- drops the six entry columns and deletes its own version row, so the roll-forward runs that fill
-- again, and that fill numbers the roles the older daemon handed work by its tokens, which no root
-- architect holds. With this row left standing, Migrate would not run this file again: every root
-- architect would read entry 0 on its row and given 0 on its claim, and the sweep's raise for an
-- architect row that names no entry (admit's resumeSlottedWorkers,
-- workflow.RaiseArchitectEntryAndStart) would hand every live architect a first task at once,
-- which is the very thing this fill exists to prevent. With it deleted, store.apply finds no row
-- of 20261113182000 and runs this file again, after the phase entry's own fill, since Migrate
-- applies the versions in order, and it settles the roots standing at the roll-forward as it
-- settled the roots of the first upgrade.
--
-- The outbox delete drops the architect-stalled wakes this release wrote and had not sent when
-- the daemon stopped. A sent wake leaves no row, since its row is deleted once it is published,
-- so these are the only ones standing. The older daemon cannot send one: it decodes a controller
-- notice strictly and refuses a kind it does not know (record.DecodeOutboxPayload, "unknown
-- controller notice kind"), and its runner retries such a row on its backoff for ever, failing at
-- every try. What the drop costs is what the roll-forward does with those wakes. A wake of the
-- first or the third arm is not raised again: the fill marks both arms for every root it settles,
-- as it does at a first upgrade. A wake of the second arm is: the fill marks that arm for no
-- root, so the first pass wakes the controller again for a root whose claim is still failed or
-- retired then.
--
-- The drop of the table and the delete of 20261113181500's row are that migration's way back, and
-- its header says why they go together. The roll-forward makes the marks' table again, empty,
-- before this file runs again.
--
-- Each statement below runs a second time cleanly on that record. The phases insert moves a row
-- only from entry 0, which is what the re-added column holds, and the claims update moves a claim
-- only from given entry 0, likewise. The marks are keyed by the run each root stands at
-- (architect_stalled_notices' primary key, which names the generation), so a root the older
-- daemon re-admitted inside the window is marked at the generation it now stands at.
--
-- What it does not cover: a root the older daemon admits after this migration has run and before
-- the new daemon serves. That promotion raised no entry, so its architect holds a claim with no
-- phases row and this fill never saw it. The first sweep of the new daemon raises that entry and
-- sends the architect its first-act task (admit's resumeSlottedWorkers,
-- workflow.RaiseArchitectEntryAndStart), which is the right outcome for such a root and is not
-- what the fill is for. A rollback and its roll-forward are no such window: the roll-forward runs
-- this fill again over every root standing then.

-- The row: entry 1 for the architect of each tree root the daemon holds a claim of. The columns
-- with no default of their own (0004_record.up.sql) take what the daemon itself writes for a row
-- it has raised no work on (workflow's enter, over record.PhaseRow's zero value). A row the record
-- already holds is moved to entry 1 only from 0, so a number the record carries is never written
-- over.
insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry)
select i.key, 'architect', '', '', 0, '', 1
from issues i
where i.key = i.tree
  and exists (select 1 from claims c where c.issue = i.key and c.role = 'architect')
on conflict (issue, role) do update set entry = 1 where phases.entry = 0;

-- The claim: queued that entry and confirming it, which is what says its architect holds the work
-- its row names. A claim already carrying a given entry is left where it stands.
update claims c set given_entry = 1, served_entry = 1
from issues i
where i.key = c.issue and i.key = i.tree and c.role = 'architect' and c.given_entry = 0;

-- The two arms this release will not wake anybody for, marked told at the run each root stands at,
-- as record.Store.TellArchitectStalled writes them.
insert into architect_stalled_notices (issue, tree, generation, arm, told_at)
select i.key, i.tree, i.generation, arm, now()
from issues i
  join claims c on c.issue = i.key and c.role = 'architect'
  cross join unnest(array['took-nothing', 'slow-before-gate']) as arm
where i.key = i.tree
on conflict do nothing;
