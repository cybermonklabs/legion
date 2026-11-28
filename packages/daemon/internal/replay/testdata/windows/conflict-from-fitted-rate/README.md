# The window `conflict-from-fitted-rate`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
four hours, from 2001-03-05T09:00Z to 13:00Z, an admission cap of 2, two issues, recorded under the rule `legion-v20.42.5` (a conflicting head moves back to implementing by `conflict`, as release 20.42.5 did).
It holds one fact of check k1: a conflict comes from the rate fitted on the window, not from the recorded move (spec R2).
`merges/acme__widgets.jsonl` holds the base branch's two first-parent commits, the two merges of the window.
`expect.json` holds the scorecards of a replay under each of three conflict paths, `legion-v20.42.5`, `main` and `legion-v22.14.1`, on the main branch, worked by hand below.
The draw is certain at the precision "The fit" below gives, so every seed gives them.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root whose head conflicts with its base once ACME-2 merges | `a0` |
| ACME-2 | a root that merges first, at 10:45 | `a1` |

## What happens (the record, under `legion-v20.42.5`)

| time (UTC) | ACME-1 | ACME-2 |
| --- | --- | --- |
| 09:00 | filed, `todo`, `legion`; promoted, slot 1 of 2; architect turn to 09:15 | |
| 09:05 | | filed and promoted, slot 2 of 2; architect turn to 09:20 |
| 09:15 / 09:20 | design registered: planning, turn to 09:30 | design registered: planning, turn to 09:35 |
| 09:30 / 09:35 | implementing, turn to 09:45 | implementing, turn to 09:50 |
| 09:45 / 09:50 | testing (stretch 1 begins), turn to 10:00 | testing (its stretch begins), turn to 10:05 |
| 10:00 / 10:05 | reviewing, turn to 10:15 | reviewing, turn to 10:20 |
| 10:15 / 10:20 | accepting, slot back; architect turn to 10:30 | accepting, slot back; architect turn to 10:30 |
| 10:30 | retro, turn to 10:45 | retro, turn to 10:40 |
| 10:40 | | merging, merger turn to 10:45 |
| 10:45 | merging, merger turn to 11:00 | `pull_request_merged`: done; its merge lands commit `5d2c8a1e…` on `main` |
| 11:00 | `conflict`: merging to implementing (stretch 1 ends in a conflict); the implementer's start takes a free slot in the same pass (`admission gives a slot back to an issue whose work came back`), turn to 11:30 | |
| 11:30 | testing (stretch 2 begins), turn to 11:45 | |
| 11:45 | reviewing, turn to 12:00 | |
| 12:00 | accepting, slot back; architect turn to 12:15 | |
| 12:15 | retro, turn to 12:30 | |
| 12:30 | merging (the conflict loop is back in the phase it left), merger turn to 12:45 | |
| 12:45 | `pull_request_merged`: done; its merge lands commit `8b1e4f7a…` | |

The work segments of ACME-1: 15 minutes each, from 09:00 to 11:00 and from 11:00 to 12:45 but the conflict round's implementing, 11:00 to 11:30, 30 minutes.
ACME-2's: 15 minutes each from 09:05 to 10:20, then accepting 10, retro 10 and merging 5 minutes.

## The fit

The stretches, each a visit to the phases from testing to merging, with M the commits of `main`
strictly after its start and strictly before its end:

| stretch | from | to | ends in | commits between | M | log2(1 + M) |
| --- | --- | --- | --- | --- | --- | --- |
| ACME-1, first | 09:45 | 11:00 | a conflict | 10:45 | 1 | 1 |
| ACME-2 | 09:50 | 10:45 | its merge | none (its own merge is its end) | 0 | 0 |
| ACME-1, second | 11:30 | 12:45 | its merge | none (its own merge is its end) | 0 | 0 |

With one covariate taking the values 0 and 1, the maximum-likelihood chance at each value is the share of conflicts among the stretches at it: 0 of 2 at M = 0, 1 of 1 at M = 1.
The stretches separate perfectly, so the fitted chance is 0 for a stretch with no merge between its ends and 1 for a stretch with one (b0 → −∞, b0 + b1 → +∞), which the tool's fit must reach to within 10⁻⁶.
A draw u from the seeded source then makes a conflict where u < 1 − 10⁻⁶ at M = 1 and where u < 10⁻⁶ at M = 0:
each outcome below differs with a chance under 10⁻⁶ whatever the seed.

The window's one recorded conflict loop runs from the 11:00 move back to ACME-1's return to merging at 12:30:
implementing 30, testing 15, reviewing 15, accepting 15 and retro 15 minutes.
Its implementing round is 30 minutes.

## The replay under `legion-v20.42.5`

The recorded conflict move is not injected.
ACME-1's first stretch runs as recorded (its segments start at their recorded worker starts with n = m), and at its recorded end, 11:00, it draws with M = 1 (ACME-2's merge at 10:45):
a conflict, which moves ACME-1 back to implementing by `conflict` at 11:00;
the loop's work is drawn from the recorded loops, of which there is one, so it is ACME-1's own: implementing 11:00 to 11:30, back in merging at 12:30.
ACME-2's stretch draws at 10:45 with M = 0 and its merge happens as recorded; ACME-1's second stretch draws at 12:45 with M = 0 and it merges at 12:45.
The replay is the record.

## The replay under `main`

`main` keeps a conflicting head in its phase and merges the base forward; in merging the merger merges it (cybermonklabs/legion#4979).
Up to 11:00 nothing differs: ACME-1's first stretch draws at 11:00 with M = 1, a conflict.
ACME-1 stays in merging; a merge-forward segment follows, as long as the recorded loops' implementing round (assumption 23), 30 minutes, its merger in a turn from 11:00 to 11:30;
then ACME-1 goes on with the work the record shows after its loop came back to merging, its last merging segment of 15 minutes, its merger in a turn from 11:30 to 11:45; and it merges at 11:45.
The stretch is not drawn again.
ACME-1 holds no slot after 10:15 (merging keeps none under `main`).
ACME-2 is as recorded.

## The replay under `legion-v22.14.1`

`legion-v22.14.1` keeps a conflicting head in its phase in every phase, and asks the implementer to merge the base forward at once, in merging too (cybermonklabs/legion#4464 without #4979, the conflict path of `bafd3618`).
Up to 11:00 nothing differs: ACME-1's first stretch draws at 11:00 with M = 1, a conflict.
ACME-1 stays in merging, and the engine asks its implementer to merge the base forward and tells its merger to wait.
A merge-forward segment follows, as long as the recorded loops' implementing round (assumption 23), 30 minutes, its implementer in a turn from 11:00 to 11:30.
The merge forward writes no supervise row and takes no slot.
Then ACME-1 goes on as under `main`: its last merging segment of 15 minutes, its merger in a turn from 11:30 to 11:45, and it merges at 11:45.
The stretch is not drawn again.
ACME-1 holds no slot after 10:15 (merging keeps none under `legion-v22.14.1`, whose slot choices are those of `main`).
No conflict is left to the merger.
ACME-2 is as recorded.

## The scorecard under `legion-v20.42.5`, worked by hand

The span is 4 hours.

**Slots.**
ACME-1 09:00 to 10:15 (75 minutes) and 11:00 to 12:00 (60), ACME-2 09:05 to 10:20 (75): 3 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 09:05.
Each was taken in the pass where its issue began to wait (its filing;
the 11:00 move back, with a slot free since 10:15), so the waits are 0, 0 and 0 and the median is 0.

**Slot-hours.**
Every slot is held in a turn: ACME-1's five 15-minute turns 09:00 to 10:15, its implementer, tester and reviewer 11:00 to 12:00, ACME-2's five turns 09:05 to 10:20.
210 minutes: `slotHoursInTurn` 3.5 hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 12:45 (225 minutes), ACME-2 09:05 to 10:45 (100): 325 minutes, 5.41666… hours, an average of 5.41666…/4 = 1.354166…, at most 2.

**Done and work left.**
ACME-2 done at 10:45, ACME-1 at 12:45 (`done` 2); `workLeft` 0.

**Started work without a slot.**
ACME-1's start after the conflict took its slot in the pass that owed it: readings (a) and (b) are 0.

**Past the review.**
ACME-1 10:15 to 11:00 (45 minutes) and 12:00 to 12:45 (45), ACME-2 10:20 to 10:45 (25), none with a slot:
`pastReviewNotHolding` 115 minutes, 1.91666… hours, an average of 0.479166…, and 2 at once from 10:20 to 10:45; `pastReviewHolding` 0.

**Priority taken.**
No issue waits for a slot: 0.

**Loops.**
One round of `conflict`, from the move back at 11:00 to ACME-1's return to merging at 12:30: 1.5 hours.

**Waits on another issue.**
None.

**Assumptions.**
1; 2 (both past their review); 3 (a send-back loop, and a drawn conflict); 9; 13 (the loop moves ACME-1 back); 18 (the window records a conflict).
`assumptions` is [1, 2, 3, 9, 13, 18]; `scenario` is `none`.

## The scorecard under `main`, worked by hand

**Slots.**
ACME-1 09:00 to 10:15 (75 minutes), ACME-2 09:05 to 10:20 (75): 2 slots on 2001-03-05, waits 0 and 0, median 0; `firstSlot` as above.

**Slot-hours.**
150 minutes in a turn, 2.5 hours; none without.
The merge forward and the last merging segment run with no slot.

**In flight.**
ACME-1 09:00 to 11:45 (165 minutes), ACME-2 09:05 to 10:45 (100): 265 minutes, 4.41666… hours, an average of 1.104166…, at most 2.

**Done and work left.**
ACME-2 at 10:45, ACME-1 at 11:45 (`done` 2); `workLeft` 0.

**Started work without a slot.**
0 in both readings.

**Past the review.**
ACME-1 10:15 to 11:45 (90 minutes), ACME-2 10:20 to 10:45 (25), with no slot:
115 minutes, 1.91666… hours, an average of 0.479166…, at most 2; `pastReviewHolding` 0.

**Priority taken.** 0.

**Loops.**
One round of `conflict`, its hours the merge-forward segment: 0.5 hours.

**Waits.**
None.

**Assumptions.**
1; 2; 3 (a drawn conflict); 9; 18 (the window records a conflict); 23 (the conflict drawn under `main` merges forward).
Nothing moves ACME-1 back, so not 13.
`assumptions` is [1, 2, 3, 9, 18, 23]; `scenario` is `none`.

## The scorecard under `legion-v22.14.1`, worked by hand

Each measure is the one under `main`: the two paths give the same times, and only the role of the merge-forward segment differs, the implementer in place of the merger.

**Slots.**
ACME-1 09:00 to 10:15 (75 minutes), ACME-2 09:05 to 10:20 (75): 2 slots on 2001-03-05, waits 0 and 0, median 0; `firstSlot` as above.

**Slot-hours.**
150 minutes in a turn, 2.5 hours; none without.
The implementer's merge forward and the last merging segment run with no slot.

**In flight.**
ACME-1 09:00 to 11:45 (165 minutes), ACME-2 09:05 to 10:45 (100): 265 minutes, 4.41666… hours, an average of 1.104166…, at most 2.

**Done and work left.**
ACME-2 at 10:45, ACME-1 at 11:45 (`done` 2); `workLeft` 0.

**Started work without a slot.**
0 in both readings.

**Past the review.**
ACME-1 10:15 to 11:45 (90 minutes), ACME-2 10:20 to 10:45 (25), with no slot:
115 minutes, 1.91666… hours, an average of 0.479166…, at most 2; `pastReviewHolding` 0.

**Priority taken.** 0.

**Loops.**
One round of `conflict`, from the ask at 11:00 to the landing at 11:30, its hours the merge-forward segment: 0.5 hours.

**Waits.**
None.

**Assumptions.**
1; 2; 3 (a drawn conflict); 9; 18 (the window records a conflict); 23 (the conflict drawn merges forward).
Nothing moves ACME-1 back, so not 13.
`assumptions` is [1, 2, 3, 9, 18, 23]; `scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under each rule that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
Each run limits all five signals (`limits`), and run.json echoes them; no rule here admits by signals, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
