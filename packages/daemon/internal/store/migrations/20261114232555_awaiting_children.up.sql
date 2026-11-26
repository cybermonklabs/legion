-- 20261114232555_awaiting_children.up.sql -- a parent that holds a phase start because a Dispatch
-- child of it is still open waits in a phase of its own, awaiting_children (dispatch://INFRA-3320
-- R11 to R13).
--
-- The phase check gains awaiting_children beside the values 20261029120700_merging_closes_the_issue
-- last wrote, so a write that moves an issue into the phase is accepted.
--
-- The held start, all null in every phase but awaiting_children: children_wait_start is the phase
-- the parent held when it entered the wait (planning, implementing, testing, reviewing or
-- accepting), children_wait_task the task its transition wrote, children_wait_reason that
-- transition's reason, children_wait_since when the wait on its children began, and
-- children_wait_children the key of every Dispatch child that held the wait: the open children
-- the move named, and each one a later admission pass found open under the waiting parent. The
-- daemon resumes the parent with these when no open child is left, and names each of those
-- children that merged during the wait in the task the return starts (record.ChildrenWait, R12).
-- The shape check ties them to the phase, as 0057_awaiting_person ties the awaiting_* columns: in
-- the phase the first four are set and held_from is null, and in every other phase all five are
-- null. children_wait_children may be null in the phase, a wait that records no child.
--
-- One column outlives the phase. children_waited_at is when the parent last moved into
-- awaiting_children, which the engine's late-completion guard compares a phase entry against
-- after the return (leftItsPhase), so it must survive the start the wait held being consumed. It
-- is read and written outside the shape check, since it is a fact about the parent's run and not
-- about the phase it stands in now.
--
-- phases.entry_from is the phase the start that raised a role's entry moved the issue from
-- (record.PhaseRow.EntryFrom), written beside phases.entry, entry_task, entry_raised_at and
-- entry_moved_back in the transaction that raises the entry (workflow's start and enter), and
-- null where no move raised it: the promotion's start of a mid-phase worker, and every row this
-- migration finds. The admission pass leaves a parent standing on an entry raised from retro or
-- merging where it stands, and a retry of its hold restarts the phase on it, since the start a
-- conflicting head writes out of an accepted phase is none R9 holds (workflow's acceptedEntry,
-- StopForChildren, retryPhase). An earlier release names the column in no
-- statement: its inserts leave it null, and its upserts leave it as it is, so an entry that
-- release raises keeps the phase of the entry before it; the rollback section of
-- docs/kubernetes.md named below clears the column for that reason.
--
-- phases.entry_given_task, phases.entry_given_entry and phases.entry_given_generation are the
-- last task a phase worker was given for an entry, as it was composed, that entry, and the
-- issue's generation it was given in (record.PhaseRow.Given, one record.GivenTask): the work a
-- raise of the entry was composed with (workflow's enter), or the task of a retry of the entry,
-- which names the retry's reason (workflow's restart: the architect's retry of a held phase, a
-- todo on a held root and the operator's restart). They are written for phase workers alone, and
-- an architect's row keeps them at '', 0 and 0.
-- phases.entry_task stays the text the role was sent when the entry was raised, which carries the
-- daemon's decorations of a send: the base-merge sentence that ends a testing task of a parent
-- and the reviewer's two sentences of its review round. A generation's clear resets the three
-- columns beside entry_task, since a member of a re-admitted tree keeps its own generation. Their
-- one reader is the admission pass's stop of a parent whose start is written and not delivered or
-- whose worker is in the middle of its phase (workflow's entryWork, StopForChildren): it holds the
-- given task where entry_given_entry is the row's entry and entry_given_generation the issue's
-- generation (record.PhaseRow.GivenFor), and the return decorates what it holds once.
--
-- The migration leaves every row it finds at '', 0 and 0 and fills nothing in: the stop reads such
-- a row, and every row whose given task is not of its entry in the issue's generation, as one
-- whose entry the release before raised. That release stored entry_task as it was composed for
-- every role but the reviewer, since it adds no base-merge sentence, so the hold is entry_task:
-- the work the entry was raised with, and not the task and reason of a retry that release made,
-- which it kept on the queued start row and the claim's pending delivery, neither of which the
-- stop reads. It stored the reviewer's entry_task with the round's two sentences, so the
-- reviewer's hold is its resume task (workflow's resumeText), which carries neither, and the
-- return names the round once; so is the hold of a row whose entry_task a clear emptied. An
-- earlier release names the three columns in no statement: its inserts leave them '', 0 and 0,
-- and its upserts leave them as they are. So an entry it raises leaves entry_given_entry at an
-- entry before, and its generation clear of an issue it reruns, adopts or re-admits as a root
-- leaves entry_given_generation below the issue's new generation, each of which the stop passes
-- over, so a rollback that stops at or above 20261111225552_phase_entry clears none of them. Its
-- clear of a re-admitted tree's member, which keeps its generation, leaves the given task readable
-- beside the emptied entry_task; docs/kubernetes.md's rollback section named below says what that
-- costs. A rollback below 20261111225552_phase_entry clears the three, by that section's statement
-- for it: that migration's way back drops phases.entry, and the roll forward runs it again, which
-- puts every role the earlier release handed work at entry 1 again, so a given task of entry 1
-- from before the rollback would match its row again in the same generation of its issue. A role
-- the earlier release handed no work stays at entry 0 after the roll forward, so a given task of
-- entry 0, which a restart of a row at entry 0 writes, would match its row the same way; so the
-- statement clears every row whose three columns are not at '', 0 and 0.
--
-- A daemon of an earlier release cannot run on this schema while a parent is in awaiting_children:
-- it does not know the phase, and a write of such a row that leaves the phase without clearing the
-- held-start columns fails the shape check below. It names children_wait_children and
-- children_waited_at in no statement: its reads leave them out, its inserts leave them null, and
-- its upserts leave them as they are. docs/kubernetes.md's section "Rolling back the release that
-- holds a parent's start while a child of it is open" says how a rollback waits for no parent to
-- stand in the phase, or moves each one out of it to the start this release's return would have
-- given it, clearing the held start and children_waited_at, and what an operator then starts by
-- hand.
--
-- There is no down file: migrations here are forward-only.
alter table issues drop constraint issues_phase_check;
alter table issues add constraint issues_phase_check check (phase in (
  'admitted', 'awaiting_person', 'awaiting_children', 'planning', 'implementing', 'testing',
  'reviewing', 'accepting', 'retro', 'merging', 'verifying', 'done', 'held'));

alter table issues add column children_wait_start text check (children_wait_start <> '');
alter table issues add column children_wait_task text check (children_wait_task <> '');
alter table issues add column children_wait_reason text check (children_wait_reason <> '');
alter table issues add column children_wait_since timestamptz;
alter table issues add column children_wait_children text[];
alter table issues add constraint issues_children_wait_shape check (
  case when phase = 'awaiting_children'
    then children_wait_start is not null and children_wait_task is not null
      and children_wait_reason is not null and children_wait_since is not null
      and held_from is null
    else children_wait_start is null and children_wait_task is null
      and children_wait_reason is null and children_wait_since is null
      and children_wait_children is null
  end);

alter table issues add column children_waited_at timestamptz;

alter table phases add column entry_from text check (entry_from <> '');
alter table phases add column entry_given_task text not null default '';
alter table phases add column entry_given_entry bigint not null default 0;
alter table phases add column entry_given_generation bigint not null default 0;
