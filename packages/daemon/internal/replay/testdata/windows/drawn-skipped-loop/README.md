# The window `drawn-skipped-loop`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
four hours, from 2001-03-05T09:00Z to 13:00Z, an admission cap of 1, three issues, recorded under the rule `legion-v20.42.5` (a conflicting head moves back to implementing by `conflict`).
It holds one fact of check k1: on the branch that draws every conflict, a stretch that goes on past a skipped conflict loop draws no second time (spec R2).
`merges/acme__widgets.jsonl` holds the base branch's four first-parent commits; the window holds no git input, so M counts every commit of the base strictly inside a stretch (`../README.md`, "Conflicts").
`expect.json` holds the scorecard of a replay under `main` on the branch `main+conflicts=drawn`, with the line of the one draw of ACME-1's stretch, worked by hand below.
Each draw is certain at the precision "The fit" below gives, so every seed gives the same values.

## The issues

| issue | what it is | rank | priority |
| --- | --- | --- | --- |
| ACME-3 | a root that takes the free slot at its filing, at 09:00 | `a0` | none |
| ACME-2 | a root filed at 09:01, which the record starts second | `a1` | none |
| ACME-1 | a root filed at 09:02, which the record starts last and whose review ends in a conflict | `a2` | 2 (P2) |

## What happens (the record, under `legion-v20.42.5`)

`legion-v20.42.5` orders the line by rank alone, so the slot goes to ACME-2 before ACME-1.

| time (UTC) | fact |
| --- | --- |
| 09:00 | the daemon boots; ACME-3 filed and promoted, slot 1 of 1; five minutes in each phase: planning 09:05, implementing 09:10, testing 09:15, reviewing 09:20 |
| 09:01, 09:02 | ACME-2, then ACME-1, filed: both wait for the slot |
| 09:25 | ACME-3 accepting, its slot back; ACME-2 promoted in the same pass, five minutes in each phase: planning 09:30, implementing 09:35, testing 09:40, reviewing 09:45 |
| 09:30, 09:35, 09:40 | ACME-3 retro, merging, merged: its merge lands `a3465b23…` at 09:40 |
| 09:50 | ACME-2 accepting, its slot back; ACME-1 promoted in the same pass, 15 minutes in each phase: planning 10:05, implementing 10:20 |
| 09:55, 10:00, 10:05 | ACME-2 retro, merging, merged: its merge lands `bd9059e7…` at 10:05 |
| 10:35 | ACME-1 testing (stretch 1 begins); 10:50 reviewing |
| 10:55 | a commit of no issue of the window, `86492b0f…`, lands on `main` |
| 11:05 | ACME-1 `conflict`: reviewing to implementing, on its slot (stretch 1 ends in a conflict); implementer turn to 11:20 |
| 11:20 | ACME-1 testing (stretch 2 begins); 11:35 reviewing (the conflict loop is back in the phase it left) |
| 11:50 | ACME-1 accepting, its slot back; 12:05 retro; 12:20 merging |
| 12:35 | ACME-1 merged: its merge lands `9999573b…` |

ACME-1's conflict loop is implementing 11:05 to 11:20 and testing 11:20 to 11:35; its implementing round is 15 minutes.

## The fit

| stretch | from | to | ends in | commits strictly between | M |
| --- | --- | --- | --- | --- | --- |
| ACME-3 | 09:15 | 09:40 | its merge | none (its own merge is its end) | 0 |
| ACME-2 | 09:40 | 10:05 | its merge | none (09:40 is its start, 10:05 its end) | 0 |
| ACME-1, first | 10:35 | 11:05 | a conflict | 10:55 | 1 |
| ACME-1, second | 11:20 | 12:35 | its merge | none | 0 |

The stretches separate: 0 of 3 conflicted at M = 0 and 1 of 1 at M = 1.
The fitted chance is 0 at M = 0 and 1 at M = 1 or more, each to within 10⁻⁶, as in `conflict-from-fitted-rate`.
No phase takes a term of its own: the one stretch that ends in reviewing conflicted, and the three that end in merging did not.

## The replay under `main`, on `main+conflicts=drawn`

`main` orders the line by priority first, so ACME-1, of priority 2, starts ahead of ACME-2, which has none, and its work comes 25 minutes sooner than in the record.

- 09:00: ACME-3 is promoted and runs as recorded: accepting at 09:25, merged at 09:40.
  Its stretch, 09:15 to 09:40, holds no commit: its draw at 09:40 is clean.

- 09:25: ACME-3's slot comes back with ACME-2 and ACME-1 waiting, and ACME-1 takes it.
  Its segments keep their lengths: planning 09:40, implementing 09:55, testing 10:10 (its stretch begins), reviewing 10:25.

- 10:40: the reviewing segment ends, where the record ended the stretch in a conflict, and the stretch draws.
  M counts the commits strictly between 10:10 and 10:40: the commit of 10:55 is not yet there, and the merges of 09:40 and 10:05 came before.
  M = 0, the chance is 0, and the draw is clean (`merges` 0, `outcome` `clean`).
  The stretch goes on past the recorded loop: the loop's implementing and testing are passed over, and the work the record shows after the loop came back to reviewing opens at once, the reviewing segment of 15 minutes, 10:40 to 10:55.

- 10:55: `review_approved`: ACME-1 is in accepting and its slot goes back; ACME-2 takes it in the same pass.
  ACME-1: retro 11:10, merging 11:25, merged at 11:40.
  The merge ends the stretch that began at 10:10, which drew at 10:40 and draws no more.
  A tool that drew again at 11:40 would count the commit of 10:55, strictly between 10:10 and 11:40: M = 1, a chance of 1, a conflict in merging, a merge forward of the loop's 15 minutes, and a merge at 11:55.

- 10:55: ACME-2 runs five minutes in each phase: planning 11:00, implementing 11:05, testing 11:10, reviewing 11:15, accepting 11:20 (its slot back), retro 11:25, merging 11:30, merged at 11:35.
  Its stretch, 11:10 to 11:35, holds no commit: its draw is clean.

## The scorecard under `main`, worked by hand

The span is 4 hours, 240 minutes.

**Slots.**
ACME-3 09:00 to 09:25 (25 minutes), ACME-1 09:25 to 10:55 (90), ACME-2 10:55 to 11:20 (25): 3 slots on 2001-03-05; `firstSlot` ACME-3 09:00, ACME-1 09:25, ACME-2 10:55.
The waits run from each filing: ACME-3 0, ACME-1 from 09:02 to 09:25 (23), ACME-2 from 09:01 to 10:55 (114).
Sorted 0, 23, 114: the median is 23.

**Slot-hours.**
Every slot is held in a turn, each segment's turn following the last with no gap: 140 minutes, `slotHoursInTurn` 2.3333… hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-3 09:00 to 09:40 (40 minutes), ACME-1 09:25 to 11:40 (135), ACME-2 10:55 to 11:35 (40): 215 minutes, 3.58333… hours, an average of 215/240 = 0.8958333…, and 2 at once from 09:25 to 09:40 and from 10:55 to 11:35.

**Done and work left.**
ACME-3 at 09:40, ACME-2 at 11:35, ACME-1 at 11:40 (`done` 3); `workLeft` 0.

**Started work without a slot.**
No work comes back without a slot: 0 in both readings.

**Past the review.**
ACME-3 09:25 to 09:40 (15 minutes), ACME-1 10:55 to 11:40 (45), ACME-2 11:20 to 11:35 (15), none with a slot: `pastReviewNotHolding` 75 minutes, 1.25 hours, an average of 0.3125, and 2 at once from 11:20 to 11:35; `pastReviewHolding` 0.

**Priority taken.**
By rank (`priorityTaken`): from 09:01 to 09:25 ACME-3 (`a0`) holds the slot above both waiters, 0; from 09:25 to 10:55 ACME-1 (`a2`) holds it while ACME-2 (`a1`) waits, the one pair, taken: 90 minutes of a share of 1, 90/240 = 0.375.
By priority first (`priorityTakenBanded`): from 09:02 to 09:25 ACME-3, with no priority, holds the slot while ACME-1 (P2) and ACME-2 (none, `a1`) wait: one pair of two is taken, a share of 0.5 for 23 minutes; from 09:25 ACME-1 (P2) holds it above ACME-2: 0.
11.5/240 = 0.0479166….

**Loops.**
None: the replay passes ACME-1's recorded loop over and makes no conflict.

**Waits.**
None.

**Assumptions.**
1; 2 (past the review); 9; 11 (a slot that comes back goes out in the same pass, at 09:25 and 10:55); 18 (the window records a conflict).
No loop runs and no conflict is made, so not 3 and not 13.
`assumptions` is [1, 2, 9, 11, 18]; `scenario` is `none`.
