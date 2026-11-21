# The window `priority-inversion`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: four hours,
from 2001-03-05T09:00Z to 13:00Z, an admission cap of 2, four issues, recorded under the rule
`main`. It holds one fact of check k1: a priority inversion, a slot holder ranked below an issue
that waits for a slot (spec R4's priority taken). ACME-1, ranked `a2`, takes a slot first and holds
it while ACME-3, ranked `a1`, waits; the share of pairs with the holder below the waiter changes as
ACME-4 joins the line and as slots come free, and its average over the window is 0.125.
`expect.json` holds the scorecard of a replay under `main` on the main branch, worked by hand below;
nothing is drawn, so every seed gives it.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root, filed first | `a2` |
| ACME-2 | a root, filed second | `a0` |
| ACME-3 | a root filed while the cap is full | `a1` |
| ACME-4 | a root filed while the cap is full | `a3` |

## What happens

| time (UTC) | ACME-1 (`a2`) | ACME-2 (`a0`) | the line |
| --- | --- | --- | --- |
| 09:00 | filed, promoted: slot 1 of 2, architect turn to 09:30 | | |
| 09:05 | | filed, promoted: slot 2 of 2, architect turn to 09:25 | |
| 09:25 | | planning, turn to 09:45 | |
| 09:30 | planning, turn to 10:00 | | ACME-3 filed: waits |
| 09:45 | | implementing, turn to 10:15 | |
| 10:00 | implementing, turn to 11:00 | | ACME-4 filed: waits behind ACME-3, by rank |
| 10:15 | | testing, turn to 10:35 | |
| 10:35 | | reviewing, turn to 11:00 | |
| 11:00 | testing, turn to 11:30 | `review_approved`: accepting, slot back; architect turn to 11:10 | ACME-3 promoted in the same pass: slot 2 of 2, architect turn 11:00 to 11:30 |
| 11:10 | | retro, turn to 11:20 | |
| 11:20 | | merging, turn to 11:40 | |
| 11:30 | reviewing, turn to 12:00 | | ACME-3 planning, turn 11:30 to 12:00 |
| 11:40 | | merged: done | |
| 12:00 | `review_approved`: accepting, slot back; architect turn to 12:10 | | ACME-4 promoted in the same pass: slot 2 of 2, architect turn 12:00 to 12:20; ACME-3 implementing, turn from 12:00 |
| 12:10 | retro, turn to 12:20 | | |
| 12:20 | merging, turn to 12:40 | | ACME-4 planning, turn from 12:20 |
| 12:40 | merged: done | | |

The segments: ACME-1 30, 30, 60, 30, 30, 10, 10, 20 minutes from 09:00; ACME-2 20, 20, 30, 20,
25, 10, 10, 20 from 09:05; ACME-3 admitted 11:00 to 11:30, planning 11:30 to 12:00, implementing
from 12:00, unended at 13:00; ACME-4 admitted 12:00 to 12:20, planning from 12:20, unended.

## The replay under `main`

The recorded rule: every pass falls at its recorded time and every segment starts at its recorded
worker start with n = m, so the replay is the record. The line places ACME-3 before ACME-4 by rank,
so the slot that comes free at 11:00 goes to ACME-3 and the one at 12:00 to ACME-4.

## The scorecard under `main`, worked by hand

The span is 4 hours.

**Slots.** ACME-1 09:00 to 12:00 (180 minutes), ACME-2 09:05 to 11:00 (115), ACME-3 11:00 to 13:00
(120), ACME-4 12:00 to 13:00 (60): 4 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 09:05,
ACME-3 11:00, ACME-4 12:00. The waits: 0, 0, ACME-3 from 09:30 to 11:00 (90), ACME-4 from 10:00 to
12:00 (120). Sorted 0, 0, 90, 120; the median is the mean of 0 and 90, 45.

**Slot-hours.** Every slot is held in a turn throughout: ACME-1's five turns 09:00 to 12:00 (180),
ACME-2's five 09:05 to 11:00 (115), ACME-3's architect, planner and implementer 11:00 to 13:00
(120), ACME-4's architect and planner 12:00 to 13:00 (60). 475 minutes: `slotHoursInTurn` 7.91666…
hours, `slotHoursNoTurn` 0.

**In flight.** ACME-1 09:00 to 12:40 (220), ACME-2 09:05 to 11:40 (155), ACME-3 11:00 to 13:00
(120), ACME-4 12:00 to 13:00 (60): 555 minutes, 9.25 hours, an average of 2.3125, and 3 at once from
11:00 to 11:40 and from 12:00 to 12:40.

**Done and work left.** ACME-2 11:40, ACME-1 12:40 (`done` 2); ACME-3 and ACME-4 in flight at 13:00
(`workLeft` 2).

**Started work without a slot.** No returned member is ever without a slot: 0 in both readings.

**Past the review.** ACME-2 11:00 to 11:40, ACME-1 12:00 to 12:40, with no slot:
`pastReviewNotHolding` 80 minutes, 1.3333… hours, an average of 0.3333…, at most 1;
`pastReviewHolding` 0.

**Priority taken.** The pairs of a holder and an issue that waits for a slot, and the share in which
the waiter ranks above the holder:

| from | to | holders | waiting | pairs with the holder below | share |
| --- | --- | --- | --- | --- | --- |
| 09:00 | 09:30 | ACME-1, ACME-2 | none | no pair | 0 |
| 09:30 | 10:00 | ACME-1 (`a2`), ACME-2 (`a0`) | ACME-3 (`a1`) | (ACME-1, ACME-3) of 2 | 1/2 |
| 10:00 | 11:00 | ACME-1 (`a2`), ACME-2 (`a0`) | ACME-3 (`a1`), ACME-4 (`a3`) | (ACME-1, ACME-3) of 4 | 1/4 |
| 11:00 | 12:00 | ACME-1 (`a2`), ACME-3 (`a1`) | ACME-4 (`a3`) | none of 2 | 0 |
| 12:00 | 13:00 | ACME-3, ACME-4 | none | no pair | 0 |

The average over the 4 hours is (0.5 × 0.5 + 1 × 0.25)/4 = 0.5/4 = 0.125 (`priorityTaken`).

**Loops and waits.** None.

**Assumptions.** 1; 2 (both past their review); 9; 11 (ACME-2's slot, freed at 11:00, to ACME-3;
ACME-1's, freed at 12:00, to ACME-4); 15 (ACME-3 and ACME-4 in flight at 13:00). `assumptions` is
[1, 2, 9, 11, 15]; `scenario` is `none`.
