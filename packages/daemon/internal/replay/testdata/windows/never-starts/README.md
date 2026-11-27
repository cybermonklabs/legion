# The window `never-starts`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
five hours, from 2001-03-05T09:00Z to 14:00Z, an admission cap of 2, four issues, recorded under the rule `keep-past-review`.
It holds one fact of check k1: an issue that never starts in the record, under each named scenario (spec R2, assumption 8).
ACME-4 waits in the line the whole window, since the slots stay held past the review;
replayed under `main`, the slots go back at the reviews and ACME-4 starts at 11:35, and its work comes from the scenario:
none, the work of a drawn started issue, or the work of a drawn finished issue.
`expect.json` holds one scorecard under `main` for each scenario (`none` on the main branch, `drawn` and `drawn-done` on `main+neverStarted=<scenario>`), worked by hand below.
No scenario is a bound.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root, started first, merged at 12:00 | `a0` |
| ACME-2 | a root, started second, past its review and unmerged at 14:00 | `a1` |
| ACME-3 | a root filed while the cap is full, started at 12:00 in the record | `a2` |
| ACME-4 | a root filed while the cap is full, never started in the record | `a3` |

## What happens (the record, under `keep-past-review`)

| time (UTC) | ACME-1 | ACME-2 | the line |
| --- | --- | --- | --- |
| 09:00 | filed, promoted: slot 1 of 2, architect turn to 09:30 | | |
| 09:05 | | filed, promoted: slot 2 of 2, architect turn to 09:35 | |
| 09:10, 09:20 | | | ACME-3, then ACME-4, filed: both wait |
| 09:30 / 09:35 | planning, turn to 10:00 | planning, turn to 10:05 | |
| 10:00 / 10:05 | implementing, turn to 10:30 | implementing, turn to 10:35 | |
| 10:30 / 10:35 | testing, turn to 11:00 | testing, turn to 11:05 | |
| 11:00 / 11:05 | reviewing, turn to 11:30 | reviewing, turn to 11:35 | |
| 11:30 / 11:35 | accepting, keeping its slot; architect turn to 11:40 | accepting, keeping its slot; architect turn to 12:05 | |
| 11:40 | retro, turn to 11:50 | | |
| 11:50 | merging, turn to 12:00 | | |
| 12:00 | merged: done, slot back | | ACME-3 promoted in the same pass: slot 2 of 2, architect turn 12:00 to 12:30 |
| 12:05 | | retro, turn to 12:35 | |
| 12:30 | | | ACME-3 planning, turn 12:30 to 13:00 |
| 12:35 | | merging; the merger's turn is open at 14:00 | |
| 13:00, 13:30 | | | ACME-3 implementing 13:00 to 13:30, then testing, its turn open at 14:00 |

ACME-4 never takes a slot: the record holds its filing and nothing else of it.

The segments: ACME-1 30 minutes in each phase to its review (09:00 to 11:30), then 10, 10, 10;
ACME-2 30 minutes in each phase to its review (09:05 to 11:35), accepting 30, retro 30, merging from
12:35, unended; ACME-3 admitted 30, planning 30, implementing 30 (12:00 to 13:30), testing from
13:30, unended (at least 30 minutes).

## The replay under `main`

`main` keeps no slot past the review.
ACME-1's and ACME-2's segments start at their recorded worker starts (each held its slot to its review) and keep their lengths.
At 11:30 ACME-1 reaches accepting and its slot goes back:
ACME-3, first in the line by rank, takes it, and its recorded segments run from 11:30 (admitted to 12:00, planning to 12:30, implementing to 13:00, testing from 13:00, which, its recorded length unknown, runs on to 14:00).
At 11:35 ACME-2's slot goes back and ACME-4 takes it: ACME-4 starts at 11:35.
ACME-1 merges at 12:00 as recorded, its slot long given; ACME-2 stays past its review to 14:00 with no slot.

ACME-4's work is the scenario's:

- `none`: ACME-4 takes its slot and its architect's start opens a segment with no turn and no end:
  it holds the slot from 11:35 to 14:00 with no worker in a turn.

- `drawn`: the work of one of the issues the record started in the window, ACME-1, ACME-2 and ACME-3, drawn with the seed.
  Over the 145 minutes from 11:35 to 14:00 the three give ACME-4 the same:
  ACME-1 and ACME-2 each ran 30 minutes in admitted, planning, implementing, testing and reviewing, so ACME-4 would run admitted 11:35 to 12:05, planning to 12:35, implementing to 13:05, testing to 13:35 and reviewing from 13:35, past 14:00;
  ACME-3 ran 30 minutes in admitted, planning and implementing and an unended testing, so ACME-4 would run the same three and then testing from 13:05 past 14:00.
  Under each, ACME-4 holds its slot with a worker in a turn from 11:35 to 14:00 and reaches no review: the draw is certain in every measure.

- `drawn-done`: the work of one of the issues the record finished, ACME-1 alone: the timeline of
  `drawn` drawing ACME-1.

## The scorecard under `main`, worked by hand, for each scenario

The span is 5 hours.
Every value is the same under the three scenarios but the two slot-hour values, `branch` and `scenario`.

**Slots.**
ACME-1 09:00 to 11:30 (150 minutes), ACME-2 09:05 to 11:35 (150), ACME-3 11:30 to 14:00 (150), ACME-4 11:35 to 14:00 (145):
4 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 09:05, ACME-3 11:30, ACME-4 11:35.
The waits: 0, 0, ACME-3 from 09:10 to 11:30 (140), ACME-4 from 09:20 to 11:35 (135).
Sorted 0, 0, 135, 140; the median is the mean of 0 and 135, 67.5.

**Slot-hours.**
ACME-1's five turns 09:00 to 11:30 (150 in a turn), ACME-2's five 09:05 to 11:35 (150), ACME-3's architect, planner, implementer and tester 11:30 to 14:00 (150): 450 minutes in a turn.
ACME-4's 145 minutes are with no turn under `none` and in a turn under `drawn` and `drawn-done`.
So `slotHoursInTurn` is 450 minutes, 7.5 hours, and `slotHoursNoTurn` 145 minutes, 2.41666… hours, under `none`;
under `drawn` and `drawn-done`, 595 minutes, 9.91666… hours, and 0.

**In flight.**
ACME-1 09:00 to 12:00 (180), ACME-2 09:05 to 14:00 (295), ACME-3 11:30 to 14:00 (150), ACME-4 11:35 to 14:00 (145):
770 minutes, 12.8333… hours, an average of 2.56666…, and 4 at once from 11:35 to 12:00.

**Done and work left.**
ACME-1 at 12:00 (`done` 1); ACME-2, ACME-3 and ACME-4 in flight at 14:00 (`workLeft` 3).

**Started work without a slot.**
No returned member is ever without a slot: 0 in both readings.

**Past the review.**
ACME-1 11:30 to 12:00 (30 minutes), ACME-2 11:35 to 14:00 (145), with no slot:
`pastReviewNotHolding` 175 minutes, 2.91666… hours, an average of 0.58333…, and 2 at once from 11:35 to 12:00; `pastReviewHolding` 0.

**Priority taken.**
ACME-3 (`a2`) waits from 09:10 to 11:30 and ACME-4 (`a3`) from 09:20 to 11:35, under ACME-1 (`a0`) and ACME-2 (`a1`), and ACME-4 from 11:30 to 11:35 under ACME-2 and ACME-3 (`a2`):
every holder ranks above every waiter, so `priorityTaken` is 0.

**Loops and waits.**
None.

**Assumptions.**
1; 2 (ACME-1 and ACME-2 past their review);
8 (ACME-4, handed to Legion, never started in the record);
9; 11 (ACME-1's slot, freed at 11:30, to ACME-3; ACME-2's, freed at 11:35, to ACME-4); 15 (three issues in flight at 14:00).
`assumptions` is [1, 2, 8, 9, 11, 15] under every scenario; `scenario` is `none`, `drawn` and `drawn-done` in turn.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` in each scenario that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
Each run limits all five signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted, and the drawn work's kills have no rate.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
