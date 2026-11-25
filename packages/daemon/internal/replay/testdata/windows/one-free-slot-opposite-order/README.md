# The window `one-free-slot-opposite-order`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: 90 minutes, from 2001-03-05T09:00Z to 10:30Z, an admission cap of 1, three issues of ACME and one of ACMEAUX, recorded under the rule `main`.
It holds check k1's one-free-slot case: two eligible issues below P0 stand in opposite order under the recorded rule and under a rule that orders the line by the priority Dispatch resolves, and one slot is free (spec R10: "the admission pass uses it for priority order").
The record is Dispatch's from before it served `resolved_priority`, so the replay reconstructs it from the window's parent chains.
`expect.json` holds the scorecards of a replay under `main` and under `p0-at-once`, worked by hand below; nothing is drawn, so every seed gives them.

## The issues

| issue | what it is | own priority | Dispatch parent | rank | resolved priority |
| --- | --- | --- | --- | --- | --- |
| ACMEAUX-1 | an issue of ACMEAUX that Legion does not work, filed at 09:01 | P1 | none | `a01` | P1 |
| ACME-3 | an issue Legion does not work, `backlog` from 09:05, closed by a person at 10:00 | none | none | `a03` | none |
| ACME-1 | a root handed to Legion at 09:10, `blocked_by` ACME-3 | P2 | none | `a01` | P2, its own |
| ACME-2 | a root handed to Legion at 09:15, `blocked_by` ACME-3 | none | ACMEAUX-1 | `a02` | P1, from ACMEAUX-1 |

No issue holds a slot before 10:00, so the one slot of the cap is free; ACME-1 and ACME-2 each wait on ACME-3, a blocker that is not `done`.
At 10:00 a person closes ACME-3, and that pass finds both eligible and the one slot free, so it starts the first of the two in the line's order.

## Which issue is first

- `main`, the recorded rule, orders the line by priority first, the priority of `record.PriorityOf`: the issue's own, else its tree root's.
  ACME-1's is its own P2; ACME-2 has none of its own and is its own tree root, so it has none, which stands after P3.
  ACME-1 is first, and the record starts it at 10:00: its slot, its architect's launch and the start of its first turn, open at 10:30.
- `p0-at-once` orders the line by the priority Dispatch resolves, as a rule with R10's start of a P0 issue outside the cap does.
  ACME-1's is its own P2, and ACME-2's is P1, from ACMEAUX-1, the nearest issue above it with a priority: ACME-2 is first, ahead of ACME-1's place as the record's next first start (`firstStarts=recorded`), since the line orders by the priority band before any rank.
  It starts ACME-2 at 10:00, which the record never started, so under the `none` scenario ACME-2 holds its slot to 10:30 with no turn, and ACME-1 waits for the cap to 10:30.

Each rule starts its own first issue and never the other's: a rule that starts the issue first only under the other rule fails the window.
Neither issue is P0, so neither starts outside the cap.

## The scorecards, worked by hand

The span is 90 minutes. No issue finishes, returns a slot, or enters a loop; no issue stands past its review, and the links hold issues that have not started, so no started issue waits on another.
`waitOnlyForSlot`, `startedWithoutSlot`, `pastReviewHolding` and `pastReviewNotHolding` are 0, and `loops`, `waits` and `doneAt` are empty.
One slot is given, at 10:00, to an issue whose links ended then: its wait is 0 (`medianSlotWaitMinutes` 0), it is in flight from 10:00 to 10:30 (`inFlight` 30/90 = 0.33333… on average, 0.5 hours, at most 1), and it is not done (`workLeft` 1).
Before 10:00 no issue holds a slot, so no pair of a holder and a waiting issue stands; from 10:00 to 10:30 one holder and one waiting issue make one pair.

**Under `main`.** `firstSlot` ACME-1 10:00; its slot is held in its architect's turn: `slotHoursInTurn` 0.5, `slotHoursNoTurn` 0.

- Priority taken by rank: the holder ACME-1 (`a01`) ranks above ACME-2 (`a02`): 0.
- Priority taken in today's line's order, banded by resolved priority: ACME-2, P1, ranks above the holder ACME-1, P2: 30/90 = 0.33333….
- The wait of urgent work, by resolved priority: ACME-2 alone counts, in P1, from its `todo` at 09:15; no slot and not done by 10:30 (no run-out): 75 and 75 minutes, each cut.
  ACME-1 is P2, which the reading does not count.

**Under `p0-at-once`.** `firstSlot` ACME-2 10:00; its slot is held with no turn: `slotHoursInTurn` 0, `slotHoursNoTurn` 0.5.

- Priority taken by rank: the holder ACME-2 (`a02`) ranks below ACME-1 (`a01`), which waits: 30/90 = 0.33333….
- Priority taken banded by resolved priority: the holder ACME-2, P1, ranks above ACME-1, P2: 0.
- The wait of urgent work: ACME-2, P1, 45 minutes from its `todo` at 09:15 to its slot at 10:00, and 75 to 10:30 with no `done`, cut.

**Assumptions**, under both: 1; 7 (a link holds the first start of ACME-1 and of ACME-2); 8 (ACME-2 is handed to Legion and the record never starts it); 9; 10 (the window holds the two links); 15.

## What the window shows

A replay that orders the line by each issue's own priority, or its tree root's, starts ACME-1 under `p0-at-once`, the issue that is first only under the recorded rule, and counts ACME-2 in no urgent wait.
`pending_test.go` skips the window by name until the replay reads the resolved priority (plan task t39).
