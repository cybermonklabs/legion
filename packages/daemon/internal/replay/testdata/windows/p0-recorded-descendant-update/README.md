# The window `p0-recorded-descendant-update`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: one hour, from 2001-03-05T09:00Z to 10:00Z, an admission cap of 1, two issues, recorded under the rule `main`.
It holds check k1's window that already records a cross-project descendant update from Dispatch: the child's own priority stays null while its resolved priority becomes P0 (spec R10).
The record is Dispatch's from a release that serves the resolved priority: each issue event's payload carries `resolved_priority` and `priority_from` beside `priority`, the issue's own.
`expect.json` holds the scorecards of a replay under `main` and under `p0-at-once`, worked by hand below; nothing is drawn, so every seed gives them.

## The issues

| issue | what it is | own priority | Dispatch parent | rank | recorded `resolved_priority` |
| --- | --- | --- | --- | --- | --- |
| ACME-1 | a root handed to Legion at 09:05 | none | none | `a00` | `null` |
| ACME-2 | a root handed to Legion at 09:10 | none | ACMEAUX-1, of another project | `a01` | `1` from 09:10 (`priority_from` ACMEAUX-1), `0` from 09:30 |

ACME-2's second event, at 09:30, is Dispatch's own: the `issue.updated` Dispatch appends on an issue whose resolved priority a write to another issue changed, here a person's raise of ACMEAUX-1 to P0.
Its actor is that person, its `changed` is `["resolved_priority"]`, it notifies no one, and its payload carries `priority: null`, `resolved_priority: 0` and `priority_from: "ACMEAUX-1"`.

The window holds no event of ACMEAUX-1: `window.json` names it in `missingAncestors`, as `collect` names an ancestor whose events Dispatch did not show its reader.
So the window gives ACME-2's resolved priority only as Dispatch recorded it, and the replay uses the recorded value from each event on and reconstructs nothing: a replay that reconstructs ACME-2's priority from its parent chain finds no record of ACMEAUX-1 and refuses the window, or reads ACME-2 as having none, and fails here.
ACMEAUX-1 is no required ancestor, since every event of ACME-2 carries the value Dispatch resolved.

## What happens

| time (UTC) | fact | `main`, the record | `p0-at-once` |
| --- | --- | --- | --- |
| 09:00 | the daemon boots | | |
| 09:05 | ACME-1 filed `todo`, `legion` | promoted: slot 1 of 1, architect turn from 09:05, open at 10:00 | the same |
| 09:10 | ACME-2 filed under ACMEAUX-1, resolved P1 | the cap is full: ACME-2 waits | waits: a P1 issue needs a slot of the cap |
| 09:30 | Dispatch's update of ACME-2: resolved P0 | nothing moves | ACME-2 is P0: the pass of the update starts it outside the cap, its slot held with no turn |

Under `main`, which starts an issue outside the cap on no priority but the issue's own, the replay is the record.
The record never started ACME-2, so under `p0-at-once` and the `none` scenario it takes its slot and its architect's start opens a segment with no turn and no end.

## The scorecards, worked by hand

The span is 60 minutes. No issue finishes, returns a slot, waits on another issue, or enters a loop; no issue stands past its review.
`waitOnlyForSlot`, `startedWithoutSlot`, `pastReviewHolding` and `pastReviewNotHolding` are 0, and `loops`, `waits` and `doneAt` are empty.

**Under `main`.**

- ACME-1 holds a slot from 09:05 to 10:00, 55 minutes, in a turn throughout: `slotHoursInTurn` and `inFlight` 55/60 = 0.91666…, at most 1 in flight, `slotHoursNoTurn` 0; one slot, `firstSlot` ACME-1 09:05, its wait 0; `workLeft` 1.
- Priority taken by rank: ACME-1 (`a00`) above ACME-2 (`a01`): 0.
- Priority taken in today's line's order, banded by resolved priority: ACME-2 waits from 09:10, at P1 and from 09:30 at P0, above ACME-1 with none: inverted from 09:10 to 10:00, 50/60 = 0.83333….
- The wait of urgent work: ACME-2, `todo` at 09:10 at P1 and P0 from 09:30, counts in P0, the highest before its first slot; no slot and not done by 10:00 (no run-out): 50 and 50 minutes, each cut.
- `assumptions`: 1; 8 (ACME-2 is handed to Legion and the record never starts it); 9; 10 (the window holds a Dispatch child, ACME-2 under ACMEAUX-1); 15.

**Under `p0-at-once`.**

- Slots: ACME-1 09:05 to 10:00 (55 minutes, in a turn), ACME-2 09:30 to 10:00 (30, no turn): `slotHoursInTurn` 55/60 = 0.91666…, `slotHoursNoTurn` 30/60 = 0.5; two slots, `firstSlot` ACME-1 09:05, ACME-2 09:30.
  The waits: ACME-1 0, ACME-2 20 (from its filing at 09:10): the median of two, (0 + 20)/2 = 10.
- In flight: 55 + 30 = 85 minutes, 85/60 = 1.41666… on average and in hours, at most 2; `workLeft` 2.
- Priority taken by rank: from 09:10 to 09:30 ACME-1 (`a00`) holds above ACME-2 (`a01`), and from 09:30 nothing waits: 0.
- Priority taken banded by resolved priority: from 09:10 to 09:30 ACME-2, at P1, ranks above ACME-1: 20/60 = 0.33333….
- The wait of urgent work: ACME-2, P0 at its first slot at 09:30 (the reading counts the priority at the slot's moment): 20 minutes to its slot, and 50 to the window's end with no `done`, cut.
- `assumptions`: 1, 8, 9, 10, 15.

## What the window shows

The child changes to P0 at Dispatch's recorded update, and the rule with the new P0 start starts it at that update's pass despite a full regular cap, from the recorded value alone.

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
