# The window `child-moved-out`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: seven
hours, from 2001-03-05T09:00Z to 16:00Z, an admission cap of 1, three issues, recorded under the
rule `main`. It holds one fact of check k1: a child moved out from under its parent ends the
parent's children wait at the time its event gives it (spec R2: each child has the lifetime its
Dispatch events give it). ACME-1's architect files ACME-2 under it and leaves it unreleased, in
`backlog`; when ACME-1's gate opens at 09:30 its children wait begins and its slot goes to ACME-3;
when a person moves ACME-2 out at 10:30, ACME-1 waits only for a slot until 12:00. `expect.json`
holds the scorecard of a replay under `main` on the main branch, worked by hand below; nothing is
drawn, so every seed gives it.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root handed to Legion that runs every phase to `done` | `a0` |
| ACME-2 | a child of ACME-1 its architect filed in `backlog` (not released, so no record of the daemon's, but open to Dispatch), moved out at 10:30 | `a2` |
| ACME-3 | a root handed to Legion while the cap is full | `a1` |

## What happens

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots | |
| 09:05 | ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 1, architect turn 09:05 to 09:30 |
| 09:10 | ACME-3 filed, `todo`, `legion` | the cap is full: ACME-3 waits |
| 09:20 | ACME-2 filed under ACME-1 by its architect's session, `backlog` (`events/ACME-2.jsonl`, seq 1) | the mirror records ACME-2 with its parent; the engine records no child in `backlog` (`recordChildUnderLiveTree` takes a `todo` alone) |
| 09:30 | ACME-1's architect ends its turn and registers its design (gate open at once) | ACME-1 stands at admitted under an open gate with an open child (`backlog` is open, `record.ChildOpen`): its children wait begins (`admission: an issue waits for the Dispatch children under it`, children 1) and its slot goes back (0 of 1); its first phase does not start. In the same pass ACME-3 is promoted: slot 1 of 1, architect turn 09:30 to 10:00 |
| 10:00 | ACME-3's design registered | planning; turn 10:00 to 10:30 |
| 10:30 | a person moves ACME-2 out (`parent: null`, seq 2) | ACME-1 has no open child left: its wait ends, and with the cap full it waits only for a slot, first in the returned part |
| 10:30 | ACME-3 `planner_completed` | implementing; turn 10:30 to 11:00 |
| 11:00 | ACME-3 `implementation_ready` | testing; turn 11:00 to 11:30 |
| 11:30 | ACME-3 `tester_passed` | reviewing; turn 11:30 to 12:00 |
| 12:00 | ACME-3 `review_approved` | accepting; its slot goes back, and in the same pass ACME-1 takes it (`admission gives a slot back to an issue whose wait ended`, admitted) and its own first phase starts (`workflow: the root of this tree takes its slot and starts its own first phase`): planner turn 12:00 to 12:30. ACME-3's architect turn 12:00 to 12:15 |
| 12:15 | ACME-3 `sign_off` | retro; turn 12:15 to 12:30 |
| 12:30 | ACME-3 `retro_completed`; ACME-1 `planner_completed` | ACME-3 merging, turn 12:30 to 13:00; ACME-1 implementing, turn 12:30 to 13:30 |
| 13:00 | ACME-3 `pull_request_merged` | ACME-3 done |
| 13:30 | ACME-1 `implementation_ready` | testing; turn 13:30 to 14:00 |
| 14:00 | ACME-1 `tester_passed` | reviewing; turn 14:00 to 14:30 |
| 14:30 | ACME-1 `review_approved` | accepting; its slot goes back; architect turn 14:30 to 14:45 |
| 14:45 | ACME-1 `sign_off` | retro; turn 14:45 to 15:00 |
| 15:00 | ACME-1 `retro_completed` | merging; turn 15:00 to 15:30 |
| 15:30 | ACME-1 `pull_request_merged` | done |

ACME-1's segments: admitted 09:05 to 09:30 (25 min); planning from its worker start at 12:00 (the
phase change and the slot in one pass) to 12:30 (30); implementing 60; then 30, 30, 15, 15, 30.
ACME-3's: 30, 30, 30, 30, 30, 15, 15, 30 minutes.

## The replay under `main`

The recorded rule. The move of ACME-2 out from under ACME-1 ends ACME-1's children wait, so the
main branch's `Timing = offset` places it at its recorded offset from that wait's start: the wait
starts at 09:30 in the replay as in the record (the gate's opening, later than ACME-2's filing at
09:20), and the move came 60 minutes into it, at 10:30. Every pass falls at its recorded time and
every segment starts at its recorded worker start with n = m, so the replay is the record. A replay
that kept the child under its parent for the whole window would hold ACME-1 at admitted to the end.

## The scorecard under `main`, worked by hand

The span is 7 hours.

**Slots.** ACME-1 09:05 to 09:30 (25 minutes), ACME-3 09:30 to 12:00 (150), ACME-1 12:00 to 14:30
(150): 3 slots on 2001-03-05; `firstSlot` ACME-1 09:05, ACME-3 09:30. The waits: ACME-1 at 09:05,
0; ACME-3 from 09:10 to 09:30, 20 minutes; ACME-1 from the move at 10:30 to 12:00, 90 minutes. The
median of 0, 20 and 90 is 20.

**Slot-hours.** ACME-1 09:05 to 09:30: architect, 25 minutes in a turn. ACME-3 09:30 to 12:00:
architect, planner, implementer, tester, reviewer, 150 minutes. ACME-1 12:00 to 14:30: planner,
implementer, tester, reviewer, 150 minutes. `slotHoursInTurn` 325 minutes, 5.41666… hours;
`slotHoursNoTurn` 0.

**In flight.** ACME-1 09:05 to 15:30 (385 minutes; it stays in flight while it waits), ACME-3 09:30
to 13:00 (210): 595 minutes, 9.91666… hours, an average of 1.41666…, at most 2.

**Done and work left.** ACME-3 done at 13:00, ACME-1 at 15:30 (`done` 2); `workLeft` 0. ACME-2 is
never started.

**Started work without a slot.** ACME-1 is a returned member with no slot from 09:30 to 12:00:
reading (b) 2.5 hours, an average of 0.357142…, at most 1. `admission.blocked` names it from 09:30
to 10:30 (its child); from 10:30 to 12:00 it waits only for a slot: reading (a) 1.5 hours, an
average of 0.214285…, at most 1.

**Past the review.** ACME-3 from 12:00 to 13:00 and ACME-1 from 14:30 to 15:30, with no slot:
`pastReviewNotHolding` 2 hours, an average of 0.285714…, at most 1; `pastReviewHolding` 0.

**Priority taken.** 09:10 to 09:30: ACME-3 (`a1`) waits while ACME-1 (`a0`) holds, a share of 0.
10:30 to 12:00: ACME-1 (`a0`) waits while ACME-3 (`a1`) holds, a share of 1. The average is
1.5/7 = 0.214285….
No issue of the window has a Dispatch priority, so every issue stands in the band after P3 and
the band moves no rank: `priorityTakenBanded`, the share in today's line's order, is the same
0.214285….

**Loops.** None.

**Waits on another issue.** ACME-1's children wait, 09:30 to 10:30, ended by its last open child
moving out: `waits` holds `child_moved_out` with a count of 1 and 1 hour. Before 09:30 ACME-1's
gate never opened, and a root at work on its design waits on nothing.

**Assumptions.** 1; 2 (ACME-3 and ACME-1 past their review); 4 (ACME-1's children wait); 9 (slots
given out); 10 (a child); 11 (ACME-1's slot, freed at 09:30, to ACME-3; ACME-3's, freed at 12:00, to
ACME-1); 17 (ACME-2 filed after ACME-1 started). ACME-1's first phase waits on its child, a started
issue's wait (4) rather than a hold of a first start (7). `assumptions` is [1, 2, 4, 9, 10, 11, 17];
`scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all six signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `refusals`: no minute of the provider's record before 09:00.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
