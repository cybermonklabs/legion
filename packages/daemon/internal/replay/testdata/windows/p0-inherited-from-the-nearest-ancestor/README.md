# The window `p0-inherited-from-the-nearest-ancestor`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: one hour, from 2001-03-05T09:00Z to 10:00Z, an admission cap of 1, two issues of ACME and two of the project ACMEAUX, recorded under the rule `main`.
It holds the nearest-ancestor row of check k1: a child with no priority of its own whose parent has a priority that differs from that of the top issue of its chain (spec R10: "the nearest issue above it on its Dispatch parent chain that has a priority").
The record is Dispatch's from before it served `resolved_priority`: no event carries it, so the replay reconstructs each issue's resolved priority from the window's parent chains.
`expect.json` holds the scorecards of a replay under `main`, the recorded rule, and under `p0-at-once`, a rule with R10's start of a P0 issue outside the cap, worked by hand below; nothing is drawn, so every seed gives them.

## The issues

| issue | what it is | own priority | Dispatch parent | rank | resolved priority |
| --- | --- | --- | --- | --- | --- |
| ACMEAUX-1 | an issue of ACMEAUX that Legion does not work, filed at 09:01: the top of ACME-2's chain | P2 | none | `a01` | P2, its own |
| ACMEAUX-2 | an issue of ACMEAUX that Legion does not work, filed at 09:02 | P0 | ACMEAUX-1 | `a02` | P0, its own |
| ACME-1 | a root handed to Legion at 09:05 | none | none | `a00` | none |
| ACME-2 | a root handed to Legion at 09:10 | none | ACMEAUX-2 | `a01` | P0, from ACMEAUX-2 |

ACME-2's chain is ACMEAUX-2, its parent, then ACMEAUX-1, the top.
ACME-2 has no priority of its own, and the nearest issue above it that has one is its parent, ACMEAUX-2, at P0: it resolves to P0, not to the top's P2.
ACME-2 is a root of a Legion tree of its own, since its parent stands in no tree, so its tree root's priority, which today's line reads (`record.PriorityOf`, own else tree root), is its own, none.

## What happens

| time (UTC) | fact | `main`, the record | `p0-at-once` |
| --- | --- | --- | --- |
| 09:00 | the daemon boots | | |
| 09:05 | ACME-1 filed `todo`, `legion` | promoted: slot 1 of 1, architect turn from 09:05, open at 10:00 | the same |
| 09:10 | ACME-2 filed under ACMEAUX-2 | the cap is full: ACME-2 waits | ACME-2 is P0: the pass starts it outside the cap, its slot held with no turn |

Under `main`, which starts no issue outside the cap on a priority it inherits, the replay is the record.
The record never started ACME-2, so under `p0-at-once` and the `none` scenario it takes its slot and its architect's start opens a segment with no turn and no end (`../README.md`, "The knobs").

## The scorecards, worked by hand

The span is 60 minutes. No issue finishes, returns a slot, waits on another issue, or enters a loop; no issue stands past its review.
`waitOnlyForSlot`, `startedWithoutSlot`, `pastReviewHolding` and `pastReviewNotHolding` are 0, and `loops`, `waits` and `doneAt` are empty.

**Under `main`.**

- Slots: ACME-1 from 09:05 to 10:00, 55 minutes, in a turn throughout: `slotHoursInTurn` 55/60 = 0.91666…, `slotHoursNoTurn` 0; one slot on 2001-03-05, `firstSlot` ACME-1 09:05, its wait 0, `medianSlotWaitMinutes` 0.
- In flight: ACME-1 alone, 55 minutes: `inFlight` 0.91666… on average and in hours, at most 1; `workLeft` 1.
- Priority taken by rank: ACME-1, the one holder, at `a00`, ranks above ACME-2 at `a01`: 0.
- Priority taken in today's line's order, each issue banded by its resolved priority: from 09:10 ACME-2, P0, waits above ACME-1, which has none: 50/60 = 0.83333….
- The wait of urgent work, by resolved priority: ACME-2, P0 from its `todo` at 09:10, no slot and not done by 10:00 (no run-out): 50 and 50 minutes, each cut.
  P0: 1 issue, median and 90th percentile 50 to the first slot and to `done`, 1 cut each; no issue counts in P1.
- `assumptions`: 1; 8 (ACME-2 is handed to Legion and the record never starts it); 9; 10 (the window holds Dispatch children, ACME-2 under ACMEAUX-2 and ACMEAUX-2 under ACMEAUX-1); 15.

**Under `p0-at-once`.**

- Slots: ACME-1 09:05 to 10:00 (55 minutes, in a turn), ACME-2 09:10 to 10:00 (50, no turn): `slotHoursInTurn` 55/60 = 0.91666…, `slotHoursNoTurn` 50/60 = 0.83333…; two slots, `firstSlot` ACME-1 09:05 and ACME-2 09:10, each wait 0, `medianSlotWaitMinutes` 0.
- In flight: 55 + 50 = 105 minutes: `inFlight` 105/60 = 1.75 on average and in hours, at most 2, from 09:10; `workLeft` 2.
- Priority taken by rank and in today's line's order: from 09:10 no issue waits for a slot, so no pair stands: 0 and 0.
- The wait of urgent work: ACME-2, P0, its slot at its `todo` (0 minutes), not done (50, cut).
  P0: 1 issue, 0 to the first slot with none cut, 50 to `done` with 1 cut.
- `assumptions`: 1, 8, 9, 10, 15.

## What the window shows

The rule with the new P0 start starts ACME-2 at 09:10 when only the cap holds it, by the priority of its parent, the nearest issue above it with one; and the recorded rule starts nothing outside the cap.
A replay that gives ACME-2 the priority of the top issue of its chain, ACMEAUX-1's P2, starts nothing outside the cap under `p0-at-once`, counts ACME-2 in no urgent wait under either rule, and bands it at P2 under `main`, above ACME-1 all the same: it fails each run on `firstSlot` or on `urgentWaits`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under each rule that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
Each run limits all six signals (`limits`), and run.json echoes them; neither rule here admits by signals, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `refusals`: no minute of the provider's record before 09:00.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
