# The window `p0-inherited-across-projects`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: one hour, from 2001-03-05T09:00Z to 10:00Z, an admission cap of 1, four issues of ACME and two of the project ACMEAUX, recorded under the rule `main`.
It holds the full-cap window of check k1 with a child that inherits P0 across a Dispatch parent link in another project and outside its Legion tree, an explicit P1 child under the same root, and an ancestor's change while an issue waits (spec R10).
The record is Dispatch's from before it served `resolved_priority`: no event carries it, so the replay reconstructs each issue's resolved priority from the window's parent chains.
`expect.json` holds the scorecard of a replay under `main`, a rule with R10's start of a P0 issue outside the cap since cybermonklabs/legion#5488, worked by hand below; nothing is drawn, so every seed gives it.
It held one under `p0-at-once` too, the same but for the rule's name, until the repository kept that rule no more ("What moved with cybermonklabs/legion#5488").

## The issues

| issue | what it is | own priority | Dispatch parent | rank | resolved priority |
| --- | --- | --- | --- | --- | --- |
| ACMEAUX-1 | an issue of ACMEAUX that Legion does not work, filed at 09:01 | P0 | none | `a01` | P0, its own |
| ACMEAUX-2 | an issue of ACMEAUX that Legion does not work, filed at 09:02, raised to P0 at 09:40 | P2, then P0 | none | `a02` | P2, then P0 at 09:40 |
| ACME-1 | a root handed to Legion at 09:05 | none | none | `a00` | none |
| ACME-2 | a root handed to Legion at 09:10 | none | ACMEAUX-1 | `a01` | P0, from ACMEAUX-1 |
| ACME-3 | a root handed to Legion at 09:15 | P1 | ACMEAUX-1 | `a02` | P1, its own |
| ACME-4 | a root handed to Legion at 09:20 | none | ACMEAUX-2 | `a03` | P2 from ACMEAUX-2, then P0 at 09:40 |

ACMEAUX-1 is the Dispatch root of ACME-2 and ACME-3 and stands outside their Legion trees: each of the three ACME issues under an ACMEAUX parent is a root of a Legion tree of its own, since its parent stands in no tree, and no root of a Legion tree stands above it, so no ancestor hold applies.
By the rule of Dispatch's `resolved_priority` (spec R10), an issue with no priority of its own takes the priority of the nearest issue above it that has one, in any project: ACME-2 resolves to ACMEAUX-1's P0, ACME-3 keeps its own P1, and ACME-4 resolves to ACMEAUX-2's P2 and, from ACMEAUX-2's raise at 09:40, to P0.
The tree root of each is the issue itself, whose own priority is none for ACME-2 and ACME-4, so the order of today's line (`record.PriorityOf`, own else tree root) gives them none: the root of a Legion tree is no substitute for the Dispatch chain (spec R10).

## What happens

| time (UTC) | fact | the record, `main` before cybermonklabs/legion#5488 | `main` since cybermonklabs/legion#5488 |
| --- | --- | --- | --- |
| 09:00 | the daemon boots | | |
| 09:05 | ACME-1 filed `todo`, `legion` | promoted: slot 1 of 1, architect turn from 09:05, open at 10:00 | the same |
| 09:10 | ACME-2 filed under ACMEAUX-1 | the cap is full: ACME-2 waits | ACME-2 is P0: the pass starts it outside the cap, its slot held with no turn |
| 09:15 | ACME-3 filed under ACMEAUX-1, P1 | waits | waits: a P1 issue needs a slot of the cap, which ACME-1 holds |
| 09:20 | ACME-4 filed under ACMEAUX-2 | waits | waits: P2 |
| 09:40 | a person raises ACMEAUX-2 to P0 | nothing moves | ACME-4 is P0 from this moment: the pass starts it outside the cap, its slot held with no turn |

Under the recorded rule, `main` before cybermonklabs/legion#5488, which starts no issue outside the cap on a priority it inherits, the replay is the record: every pass falls at its recorded time and ACME-1's segment starts at its recorded worker start, so nothing outside the cap starts.
Under `main` since cybermonklabs/legion#5488, the recorded event of ACMEAUX-2 at 09:40 moves ACME-4's resolved priority, as the update Dispatch appends on each descendant whose resolved priority a write changes (spec R10), so the daemon's pass of that moment, and no later one, reads ACME-4 as P0.
The record never started ACME-2 or ACME-4, so under the `none` scenario each takes its slot and its architect's start opens a segment with no turn and no end (`../README.md`, "The knobs").

## The scorecards, worked by hand

The span is 60 minutes. No issue finishes, returns a slot, waits on another issue, or enters a loop; no issue stands past its review.
`waitOnlyForSlot`, `startedWithoutSlot`, `pastReviewHolding` and `pastReviewNotHolding` are 0, and `loops`, `waits` and `doneAt` are empty.

**Under the recorded rule, `main` before cybermonklabs/legion#5488.**

- Slots: ACME-1 from 09:05 to 10:00, 55 minutes, in a turn throughout: `slotHoursInTurn` 55/60 = 0.91666…, `slotHoursNoTurn` 0; one slot on 2001-03-05, `firstSlot` ACME-1 09:05, its wait 0, `medianSlotWaitMinutes` 0.
- In flight: ACME-1 alone, 55 minutes: `inFlight` 0.91666… on average and in hours, at most 1; `workLeft` 1.
- Priority taken by rank: ACME-1, the one holder, at `a00`, ranks above each waiting issue: 0.
- Priority taken in today's line's order, each issue banded by its resolved priority: from 09:10 each waiting issue (ACME-2 P0, ACME-3 P1 from 09:15, ACME-4 P2 from 09:20 and P0 from 09:40) ranks above ACME-1, which has none, so every pair is inverted from 09:10 to 10:00: 50/60 = 0.83333….
- The wait of urgent work, by resolved priority, each wait cut at 10:00 (no run-out): ACME-2, P0 from its `todo` at 09:10, no slot: 50 and 50 minutes; ACME-4, `todo` at 09:20 at P2 and P0 from 09:40, which counts it in P0 as the highest priority before its first slot: 40 and 40; ACME-3, P1, `todo` at 09:15: 45 and 45.
  P0: 2 issues, the first-slot waits 40 and 50 (median 45, 90th percentile 50, both cut), the same to `done`; P1: 1 issue, 45 each, cut.
- `assumptions`: 1; 8 (ACME-2, ACME-3 and ACME-4 are handed to Legion and the record never starts them); 9; 10 (the window holds three Dispatch children, ACME-2 and ACME-3 under ACMEAUX-1 and ACME-4 under ACMEAUX-2); 15.

**Under `main` since cybermonklabs/legion#5488.**

- Slots: ACME-1 09:05 to 10:00 (55 minutes, in a turn), ACME-2 09:10 to 10:00 (50, no turn), ACME-4 09:40 to 10:00 (20, no turn): `slotHoursInTurn` 55/60 = 0.91666…, `slotHoursNoTurn` 70/60 = 1.16666…; three slots, `firstSlot` ACME-1 09:05, ACME-2 09:10, ACME-4 09:40.
  The waits: ACME-1 0, ACME-2 0, ACME-4 20 (from its filing at 09:20, behind the cap, to 09:40): sorted 0, 0, 20, `medianSlotWaitMinutes` 0.
- In flight: 55 + 50 + 20 = 125 minutes: `inFlight` 125/60 = 2.08333… on average and in hours, at most 3, from 09:40; `workLeft` 3.
- Priority taken by rank:

  | from | to | holders | waiting | pairs with the holder below | share |
  | --- | --- | --- | --- | --- | --- |
  | 09:10 | 09:15 | ACME-1 (`a00`), ACME-2 (`a01`) | none | no pair | 0 |
  | 09:15 | 09:20 | ACME-1, ACME-2 | ACME-3 (`a02`) | none of 2 | 0 |
  | 09:20 | 09:40 | ACME-1, ACME-2 | ACME-3, ACME-4 (`a03`) | none of 4 | 0 |
  | 09:40 | 10:00 | ACME-1, ACME-2, ACME-4 (`a03`) | ACME-3 (`a02`) | (ACME-4, ACME-3) of 3 | 1/3 |

  (20 × 1/3)/60 = 0.11111….
- Priority taken in today's line's order, banded by resolved priority:

  | from | to | holders | waiting | pairs with the holder below | share |
  | --- | --- | --- | --- | --- | --- |
  | 09:15 | 09:20 | ACME-1 (none), ACME-2 (P0) | ACME-3 (P1) | (ACME-1, ACME-3) of 2 | 1/2 |
  | 09:20 | 09:40 | ACME-1, ACME-2 | ACME-3 (P1), ACME-4 (P2) | (ACME-1, ACME-3), (ACME-1, ACME-4) of 4 | 1/2 |
  | 09:40 | 10:00 | ACME-1, ACME-2, ACME-4 (P0) | ACME-3 (P1) | (ACME-1, ACME-3) of 3 | 1/3 |

  (5 × 1/2 + 20 × 1/2 + 20 × 1/3)/60 = 19.1666…/60 = 0.31944….
- The wait of urgent work: ACME-2, P0, slot at its `todo` (0 minutes), not done (50, cut); ACME-4, P0 at its slot at 09:40, which the reading includes (20 minutes), not done (40, cut); ACME-3, P1, neither (45 and 45, cut).
  P0: 2 issues, first-slot waits 0 and 20 (median 10, 90th percentile 20, none cut), waits to `done` 40 and 50 (median 45, 90th percentile 50, both cut); P1: 1 issue, 45 each, cut.
- `assumptions`: 1, 8, 9, 10, 15.

## What moved with cybermonklabs/legion#5488

The record was made under `main` at `0d3786da`, before R10 reached the daemon.
Since cybermonklabs/legion#5488, `main` starts a P0 issue outside the cap and reads the priority Dispatch resolves (`record.SlotRule.StartsNow`), as the candidate `p0-at-once` did; the two then differed only in name, so the run under `main` is the one worked above, and the row of `p0-at-once` went from `expect.json` with that rule.
Worked from the events: at 09:10 ACME-2, P0 from ACMEAUX-1, starts outside the full cap; ACME-3, its own P1, and ACME-4, P2 from ACMEAUX-2, need a slot of the cap, which ACME-1 holds; ACMEAUX-2's raise to P0 at 09:40 makes ACME-4 P0, and that pass starts it outside the cap.
The record started neither, so each holds its slot with no turn to 10:00.

| value under `main` | before | after |
| --- | --- | --- |
| `firstSlot` | ACME-1 09:05 | ACME-1 09:05, ACME-2 09:10, ACME-4 09:40 |
| `slotsGivenByDay` | 1 on 2001-03-05 | 3 on 2001-03-05 |
| `slotHoursNoTurn` | 0 | 70/60 = 1.16666… (ACME-2 50 minutes, ACME-4 20) |
| `inFlight` | 0.91666… on average and in hours, at most 1 | 125/60 = 2.08333… on average and in hours, at most 3 |
| `workLeft` | 1 | 3 |
| `priorityTaken` | 0 | 0.11111… (the pair of ACME-4, `a03`, holding, and ACME-3, `a02`, waiting, from 09:40 to 10:00, one of three pairs) |
| `priorityTakenBanded` | 0.83333… | 0.31944… (the table above) |
| `urgentWaits.P0.firstSlot` | 40 and 50 minutes, median 45, 90th percentile 50, both cut | 0 and 20 minutes, median 10, 90th percentile 20, none cut |

`slotHoursInTurn`, `medianSlotWaitMinutes`, the waits to `done` and `urgentWaits.P1` stand as before.

## What the window shows

The rule with the new P0 start starts ACME-2 when only the cap holds it; ACME-3, an explicit P1 under the same Dispatch root, does not pass the cap; ACMEAUX-2's raise at 09:40 moves ACME-4 from P2 to P0 at that moment and starts it; and the recorded rule starts nothing outside the cap.
A replay that reads each issue's own priority, or its tree root's, as the replay did before the resolved priority, starts neither ACME-2 nor ACME-4 under `main`, and counts only ACME-3 in the urgent waits of either rule; one that takes the new P0 classification only at a later pass starts ACME-4 late.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under each rule that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
The run limits all six signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `refusals`: no minute of the provider's record before 09:00.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
