# The window `root-idle-while-children-run`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
three hours, from 2001-03-05T09:00Z to 12:00Z, an admission cap of 3, a root and two children, recorded under the rule `legion-v22.14.1`.
It holds one fact of check k1: a tree root in implementing that holds its slot with no turn while its children run.
ACME-1's planner files ACME-2 and ACME-3 under it while it plans;
each child's first phase starts at once on a slot of its own, and ACME-1 moves to implementing, where its implementer takes one turn of ten minutes and then waits with no task for the rest of the window.
In the record a root past admitted waits on no open child (the children-first wait holds a parent's first phase alone), so it keeps its slot through its children's work, and the slots reading counts it `idle` (spec R6:
no turn, nothing on its way and no named wait).
The record is of a daemon before cybermonklabs/legion#4592, so its part names `legion-v22.14.1`, the rule of release 22.14.1, as the rule it was made under: it was written under the `main` of its time, whose slot choices that release keeps, and today's `main` moves a started parent with an open Dispatch child into `awaiting_children`, so a replay under today's `main` is not the record.
`expect.json` holds the scorecards of a replay under `main` and under `legion-v22.14.1` on the main branch, worked by hand below; nothing is drawn, so every seed gives them.
The record's own scorecard is the one under `legion-v22.14.1` (`TestScorecardOfTheRecord`).

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | the root, handed to Legion | `a0` |
| ACME-2 | a child of ACME-1, filed `todo` by ACME-1's planner at 09:30 | `a1` |
| ACME-3 | a child of ACME-1, filed `todo` by ACME-1's planner at 09:35 | `a2` |

## What happens

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots | |
| 09:05 | ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 3, architect turn 09:05 to 09:20 |
| 09:20 | ACME-1's design registered (gate open) | planning by `gate_opened`; planner turn 09:20 to 09:40 |
| 09:30 | ACME-2 filed under ACME-1 by its planner's session, `todo` | recorded at admitted in ACME-1's tree; the gate is open and ACME-1 stands in planning, where no children wait holds it, so ACME-2's first phase starts at once: slot 2 of 3, planning, its architect launched, planner turn 09:30 to 10:00 |
| 09:35 | ACME-3 filed the same way | slot 3 of 3, planning, planner turn 09:35 to 10:05 |
| 09:40 | ACME-1 `planner_completed` | implementing; its implementer launched, turn 09:40 to 09:50 |
| 09:50 | ACME-1's implementer ends its turn | nothing moves: ACME-1 holds its slot in implementing with no turn and no task to the window's end |
| 10:00, 10:05 | ACME-2, ACME-3 `planner_completed` | implementing; turns of 30 minutes |
| 10:30, 10:35 | `implementation_ready` | testing; turns of 20 minutes |
| 10:50, 10:55 | `tester_passed` | reviewing; turns of 20 minutes |
| 11:10, 11:15 | `review_approved` | accepting; each child's slot goes back (2, then 1 of 3), and no issue waits for it; its architect's turn of 10 minutes |
| 11:20, 11:25 | `sign_off` | retro; turns of 10 minutes |
| 11:30, 11:35 | `retro_completed` | merging; turns of 10 minutes |
| 11:40, 11:45 | `pull_request_merged` | ACME-2, then ACME-3, done and out of the workflow |
| 12:00 | the window ends | ACME-1 in implementing, holding slot 1 with no turn since 09:50 |

ACME-1's segments: admitted 09:05 to 09:20 (15 minutes);
planning 09:20 to 09:40 (20), its planner filing ACME-2 10 minutes in and ACME-3 15 minutes in;
implementing from 09:40, unended at the window's end, its one turn the first 10 minutes.
Each child's: planning 30, implementing 30, testing 20, reviewing 20, accepting 10, retro 10 and merging 10 minutes.

## The replay under `legion-v22.14.1`

The rule of release 22.14.1, before #4592: no open child stops a started parent's phase.
Each child is filed at its recorded offset in ACME-1's planning segment, 09:30 and 09:35, and starts in the pass of its filing.
ACME-1's implementing segment is unended in the record, so in the replay it does not end before the window does, and the root holds its slot with no turn from 09:50 to 12:00.
Every pass falls at its recorded time and every segment starts at its recorded worker start with n = m, so the replay is the record.

The slots reading over this window, at any moment from 09:50 to 11:10, counts 3 holders:
ACME-2 and ACME-3 `working`, each in its worker's turn, and ACME-1 `idle` since 09:50, the end of its last turn.
ACME-1 waits on no issue it keeps its slot through (`blockers.KeptWaits`:
no link, and no open child its own first phase waits for), so it is not `issue`; it is not held, waits on no person and has nothing on its way to a worker.
The test `TestTheSlotsReadingNamesAnIdleRootAndARootWaitingOnAPerson` (`internal/admit`) reads the same shape through the daemon's own tick under every rule the repository keeps.

## The scorecard under `legion-v22.14.1`, worked by hand

The span is 3 hours.
The record was made under this rule, and `legion-v22.14.1` differs from the `main` of its time in two choices only: the start of a P0 issue and the order of the line.
The window holds no P0 issue, and no issue ever waits in the line, so neither choice moves anything, and every value below is the record's.

**Slots.**
ACME-1 09:05 to the window's end (175 minutes), ACME-2 09:30 to 11:10 (100) and ACME-3 09:35 to 11:15 (100):
3 slots on 2001-03-05 (`slotsGivenByDay`); `firstSlot` ACME-1 09:05, ACME-2 09:30, ACME-3 09:35.
Each was taken in the pass where its issue began to wait for it
— ACME-1 at its filing, each child at its filing, when its first phase could start — so the waits are 0, 0 and 0 and their median is 0.

**Slot-hours.**
ACME-1 09:05 to 12:00: architect 09:05 to 09:20, planner 09:20 to 09:40 and implementer 09:40 to 09:50, 45 minutes in a turn, then no turn from 09:50 to 12:00, 130 minutes.
ACME-2 09:30 to 11:10 and ACME-3 09:35 to 11:15: planner, implementer, tester and reviewer back to back, 100 minutes each in a turn.
`slotHoursInTurn` is 45 + 100 + 100 = 245 minutes, 4.08333… hours; `slotHoursNoTurn` is 130 minutes, 2.16666… hours, all of it ACME-1's.

**In flight.**
ACME-1 from 09:05 to the window's end (175 minutes), ACME-2 from 09:30 to 11:40 (130) and ACME-3 from 09:35 to 11:45 (130):
435 minutes, 7.25 hours, an average of 7.25/3 = 2.41666…, and 3 at once from 09:35 to 11:40 (`inFlight`).

**Done and work left.**
ACME-2 done at 11:40 and ACME-3 at 11:45 (`done` 2, `doneAt`); ACME-1 is in flight at 12:00 (`workLeft` 1).

**Started work without a slot.**
None: no issue ever stands in the line, and none is in `awaiting_children`.
Both readings are 0.

**Past the review.**
ACME-2 in accepting, retro and merging from 11:10 to 11:40 and ACME-3 from 11:15 to 11:45, neither with a slot:
`pastReviewNotHolding` 1 hour, an average of 1/3 = 0.33333…, at most 2 (11:15 to 11:40); `pastReviewHolding` 0.

**Priority taken.**
No issue ever waits for a slot: `priorityTaken` 0.

**Loops.**
None.

**Waits on another issue.**
None: ACME-1 stands past admitted while its children are open, which is no wait of the children-first rule, and no issue is linked.

**Assumptions.**
1; 2 (the children past their review); 9 (slots given out); 10 (children); 15 (ACME-1's work left at 12:00); 17 (the children filed after ACME-1 started).
No slot is freed for a waiting issue, so 11 is not named.
`assumptions` is [1, 2, 9, 10, 15, 17]; `scenario` is `none`.

## The replay under `main`

Today's rule, with cybermonklabs/legion#4592's wait: a started parent whose next start an open Dispatch child holds moves into `awaiting_children`, which keeps no slot, and comes back to the phase it held once no open child is left (`workflow.ParentWaitsLine`, `workflow.ParentReturnsLine`).

| time (UTC) | what the replay's daemon does |
| --- | --- |
| 09:05 | promotes ACME-1: slot 1 of 3, architect turn 09:05 to 09:20 |
| 09:20 | ACME-1 planning by `gate_opened`; planner turn from 09:20 |
| 09:30 | ACME-2 filed under ACME-1 by its planner's session, 10 minutes into the planning segment: ACME-1, a started parent with an open child, moves into `awaiting_children`, its planner stopped with 10 minutes of its segment left, and its slot goes back; ACME-2's first phase starts in the same pass on a slot (1 of 3) and runs its recorded course |
| 09:35 | nothing: ACME-3's filing is the planner's act 15 minutes into the planning segment, which is paused, so it moves with ACME-1's next start |
| 11:10 | ACME-2 `review_approved`: accepting, its slot goes back (0 of 3) |
| 11:40 | ACME-2 `pull_request_merged`, done: no open child is left, so ACME-1 comes back to planning and takes a slot (1 of 3); its planner resumes the rest of its segment |
| 11:45 | ACME-3 filed, 5 minutes into the rest of the segment (15 minutes in all): ACME-1 moves into `awaiting_children` again and its slot goes back; ACME-3's first phase starts on a slot (1 of 3), planner turn from 11:45 |
| 12:00 | the window ends: ACME-1 in `awaiting_children` with no slot, ACME-3 planning |

## The scorecard under `main`, worked by hand

The span is 3 hours.

**Slots.**
ACME-1 09:05 to 09:30 (25 minutes) and 11:40 to 11:45 (5), ACME-2 09:30 to 11:10 (100) and ACME-3 11:45 to the window's end (15):
4 slots on 2001-03-05 (`slotsGivenByDay`); `firstSlot` ACME-1 09:05, ACME-2 09:30, ACME-3 11:45.
Each was taken in the pass where its issue began to wait for it — ACME-1 at its filing and at its return, each child at its filing — so the waits are all 0 and their median is 0.

**Slot-hours.**
ACME-1: architect 09:05 to 09:20 and planner 09:20 to 09:30, then planner 11:40 to 11:45, 30 minutes, all in a turn.
ACME-2: planner, implementer, tester and reviewer 09:30 to 11:10, 100 minutes in a turn.
ACME-3: planner from 11:45, 15 minutes in a turn.
`slotHoursInTurn` is 30 + 100 + 15 = 145 minutes, 2.41666… hours; `slotHoursNoTurn` is 0.

**In flight.**
ACME-1 from 09:05 to the window's end (175 minutes), ACME-2 from 09:30 to 11:40 (130) and ACME-3 from 11:45 (15):
320 minutes, 5.33333… hours, an average of 5.33333…/3 = 1.77777…, and 2 at once at most (`inFlight`).

**Done and work left.**
ACME-2 done at 11:40 (`done` 1, `doneAt`); ACME-1 and ACME-3 are in flight at 12:00 (`workLeft` 2).

**Started work without a slot.**
ACME-1 in `awaiting_children` with no slot from 09:30 to 11:40 (130 minutes) and from 11:45 to 12:00 (15): a started issue before its review that holds no slot, its wait on its children included.
`startedWithoutSlot` 145 minutes, 2.41666… hours, an average of 2.41666…/3 = 0.80555…, at most 1.
No issue stands in the line's returned part, and a parent in `awaiting_children` waits on its children, never only for a slot: `waitOnlyForSlot` 0.

**Past the review.**
ACME-2 in accepting, retro and merging from 11:10 to 11:40, with no slot:
`pastReviewNotHolding` 0.5 hours, an average of 0.5/3 = 0.16666…, at most 1; `pastReviewHolding` 0.

**Priority taken.**
No issue ever waits for a slot: `priorityTaken` 0.

**Loops.**
None.

**Waits on another issue.**
ACME-1 in `awaiting_children` waits on ACME-2 from 09:30 to ACME-2's done at 11:40, one wait of `child_done` for 2.16666… hours, and on ACME-3 from 11:45 to the window's end, one wait `open` for 0.25 hours.

**Assumptions.**
1; 2 (ACME-2 past its review); 4 (ACME-1's children waits); 9 (slots given out); 10 (children); 11 (ACME-1's slot, freed at 09:30, to ACME-2 in the same pass); 15 (the work left at 12:00); 17 (the children filed after ACME-1 started).
`assumptions` is [1, 2, 4, 9, 10, 11, 15, 17]; `scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all five signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share, though the root holds its slot with no turn.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
