# The window `root-idle-while-children-run`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
three hours, from 2001-03-05T09:00Z to 12:00Z, an admission cap of 3, a root and two children, recorded under the rule `main`.
It holds one fact of check k1: a tree root in implementing that holds its slot with no turn while its children run.
ACME-1's planner files ACME-2 and ACME-3 under it while it plans;
each child's first phase starts at once on a slot of its own, and ACME-1 moves to implementing, where its implementer takes one turn of ten minutes and then waits with no task for the rest of the window.
A root past admitted waits on no open child (the children-first wait holds a parent's first phase alone), so it keeps its slot through its children's work, and the slots reading counts it `idle` (spec R6:
no turn, nothing on its way and no named wait).
`expect.json` holds the scorecard of a replay under `main` on the main branch, worked by hand below; nothing is drawn, so every seed gives it.

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

## The replay under `main`

The recorded rule.
Each child is filed at its recorded offset in ACME-1's planning segment, 09:30 and 09:35, and starts in the pass of its filing.
ACME-1's implementing segment is unended in the record, so in the replay it does not end before the window does, and the root holds its slot with no turn from 09:50 to 12:00.
Every pass falls at its recorded time and every segment starts at its recorded worker start with n = m, so the replay is the record.

The slots reading over this window, at any moment from 09:50 to 11:10, counts 3 holders:
ACME-2 and ACME-3 `working`, each in its worker's turn, and ACME-1 `idle` since 09:50, the end of its last turn.
ACME-1 waits on no issue it keeps its slot through (`blockers.KeptWaits`:
no link, and no open child its own first phase waits for), so it is not `issue`; it is not held, waits on no person and has nothing on its way to a worker.
The test `TestTheSlotsReadingNamesAnIdleRootAndARootWaitingOnAPerson` (`internal/admit`) reads the same shape through the daemon's own tick under every rule the repository keeps.

## The scorecard under `main`, worked by hand

The span is 3 hours.

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
None: no issue ever stands in the line.
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

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all six signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share, though the root holds its slot with no turn.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `refusals`: no minute of the provider's record before 09:00.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
