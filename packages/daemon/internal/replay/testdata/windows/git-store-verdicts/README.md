# The window `git-store-verdicts`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
four hours, from 2001-03-05T09:00Z to 13:00Z, an admission cap of 2, two issues, recorded under the rule `legion-v20.42.5` (a conflicting head moves back to implementing by `conflict`).
It holds two facts of check k1 on the branch that asks git (spec R2):
a stretch that the record ends in a conflict at a read that the store answers clean, so the stretch goes on past the recorded conflict loop;
and a stretch that the record ends with no conflict at a read that the store answers with a conflict, so the conflict is made and its loop's work is drawn from the window's recorded loops, of which there is one.
`expect.json` holds the scorecards of a replay under `legion-v20.42.5` (the recorded rule) and under `main`, each on `main+conflicts=git`, with the run's counts of reads and conflict-loop hours and the line of each read, worked by hand below.
Nothing is drawn from the generator, and the window holds one recorded conflict loop, so every seed gives the same values.

## The issues, the base and git's reading

| issue | pull request | what it is | rank |
| --- | --- | --- | --- |
| ACME-1 | acme/widgets#11 | a root whose review ends in a recorded conflict at 10:15, while git says its head is clean | `a0` |
| ACME-2 | acme/widgets#12 | a root whose review asks for changes at 10:20, while git says its head conflicts with #20 | `a1` |

| commit of `main` | pull request | `committed` | `at` | paths |
| --- | --- | --- | --- | --- |
| `cd085fe0…` | #20, of no issue of the window | 10:00 | 10:10 | `shelf/stock.go` |
| `a6f58ef3…` | #11, ACME-1's own | 11:45 | 11:45 | `aisle/sort.go` |
| `6317269c…` | #12, ACME-2's own | 11:50 | 11:50 | `shelf/stock.go` |

| head | stands from | merge base | paths | git's verdicts |
| --- | --- | --- | --- | --- |
| #11 `9134632e…` | 09:45 | a commit before the window | `aisle/sort.go` | none: #20 changes `shelf/stock.go` alone |
| #11 `e5007700…` | 10:30 | #20's commit | `aisle/sort.go` | none |
| #12 `00f5a4b7…` | 09:50 | a commit before the window | `shelf/stock.go` | a conflict at #20's commit |
| #12 `a9a3e0f7…` | 10:35 | #20's commit | `shelf/stock.go` | none: #11 changes `aisle/sort.go` alone |

## What happens (the record, under `legion-v20.42.5`)

Every segment is 15 minutes and starts at the move into its phase (the cap is 2, so neither issue waits).

| time (UTC) | ACME-1 | ACME-2 |
| --- | --- | --- |
| 09:00 | filed, promoted: slot 1 of 2; architect turn to 09:15 | |
| 09:05 | | filed, promoted: slot 2 of 2; architect turn to 09:20 |
| 09:15 / 09:20 | planning | planning |
| 09:30 / 09:35 | implementing | implementing |
| 09:45 / 09:50 | testing (stretch 1 begins) | testing (stretch 1 begins) |
| 10:00 / 10:05 | reviewing | reviewing |
| 10:10 | #20 lands on `main`, changing `shelf/stock.go` | |
| 10:15 | `conflict`: reviewing to implementing, on its slot (stretch 1 ends); turn to 10:30 | |
| 10:20 | | `review_rejected`: reviewing to implementing, on its slot (stretch 1 ends); turn to 10:35 |
| 10:30 / 10:35 | testing (stretch 2 begins) | testing (stretch 2 begins) |
| 10:45 / 10:50 | reviewing (the conflict loop is back in the phase it left) | reviewing |
| 11:00 / 11:05 | accepting, slot back | accepting, slot back |
| 11:15 / 11:20 | retro | retro |
| 11:30 / 11:35 | merging | merging |
| 11:45 / 11:50 | merged: `a6f58ef3…` | merged: `6317269c…` |

The window's one recorded conflict loop is ACME-1's from reviewing: implementing 10:15 to 10:30 and testing 10:30 to 10:45.
Its implementing round is 15 minutes.

## The replay under `legion-v20.42.5`, on `main+conflicts=git`

Up to 10:15 nothing differs from the record.
Every read below is read by the rules of `../README.md`, "The branch that asks git, and the generator's profile".

- **10:15, ACME-1's stretch 1 ends** (recorded move `conflict` at 10:15).
  The head that stands at 10:15 is `9134632e…`.
  The replay's base holds #20 (`shelf/stock.go`), which does not meet #11's paths (`aisle/sort.go`): no touching merge.
  The store answers clean, n = 0.
  ACME-1 skips its recorded conflict loop (implementing and testing are passed over) and opens the reviewing segment after it at once, 10:15 to 10:30.

- **10:20, ACME-2's stretch 1 ends** (recorded move `review_rejected` at 10:20).
  The head that stands at 10:20 is `00f5a4b7…`.
  The replay's base holds #20 (`shelf/stock.go`), which meets #12's paths (`shelf/stock.go`): one touching merge, `[#20]`, the first 1 of the record's verdicts of `00f5a4b7…`, whose verdict is a conflict.
  The store answers conflict, n = 1.
  Under `legion-v20.42.5` the conflict moves ACME-2 back to implementing by `conflict` on its slot, and its loop's work is drawn from the window's recorded loops, of which there is one (ACME-1's: implementing 15, testing 15), followed by ACME-2's own reviewing segment (recorded 10:05 to 10:20, ending in `review_rejected`):
  implementing 10:20 to 10:35, testing 10:35 to 10:50, reviewing 10:50 to 11:05.

- **10:30, ACME-1**: `review_approved`: accepting (its slot back); 10:45 retro; 11:00 merging.

- **11:05, ACME-2's re-entered reviewing segment ends** (recorded move `review_rejected` at 10:20).
  The head at the recorded moment 10:20 is still `00f5a4b7…`, and this read follows a conflict loop whose work the replay drew, which ended at 10:50 (ACME-2's return to reviewing).
  By the rule after a loop that the record does not hold (`../README.md`, "The branch that asks git, and the generator's profile"), ACME-2's head has merged the replay's base at 10:50, and a later read counts only commits that the replay's base took after 10:50.
  None landed between 10:50 and 11:05: no touching merge, so the store answers clean, n = 0.
  ACME-2's recorded `review_rejected` enters at 11:05, moving ACME-2 back to implementing on its slot: implementing 11:05 to 11:20, testing 11:20 to 11:35, reviewing 11:35 to 11:50, accepting 11:50 (slot back), retro 12:05, merging 12:20.

- **11:15, ACME-1's merge** (recorded move `pull_request_merged` at 11:45).
  The stretch that began at 09:45 and went on past the skipped loop ends at 11:15, and the git branch reads every stretch end.
  The head that stands at 11:45 is `e5007700…` (merge base #20); the replay's base holds no commit after #20 at 11:15 (ACME-2 has not merged): no touching merge.
  The store answers clean, n = 0, and ACME-1 merges at 11:15 (#11 enters the replay's base at 11:15).

- **12:35, ACME-2's merge** (recorded move `pull_request_merged` at 11:50).
  The head that stands at 11:50 is `a9a3e0f7…`.
  Since the drawn conflict loop ended at 10:50, the replay's base took #11 at 11:15, which changes `aisle/sort.go` alone and does not meet `shelf/stock.go`: no touching merge.
  The store answers clean, n = 0, and ACME-2 merges at 12:35.

**The counts.**
Five reads of acme/widgets: 4 clean (ACME-1 at 10:15 and 11:15, ACME-2 at 11:05 and 12:35), 1 conflict (ACME-2 at 10:20), and no unanswered read of any kind.
The conflict loop that the answered read began ran from 10:20 to 10:50: `gitLoopHours` is 0.5 answered and 0 drawn.

## The replay under `main`, on `main+conflicts=git`

ACME-1 runs as above: its read at 10:15 is clean, its slot goes back at 10:30, and its merge read at 11:15 is clean (#11 enters the base at 11:15).

- **10:20, ACME-2's stretch 1 ends.**
  The store answers conflict, n = 1, as above.
  Under `main`, a conflicting head in reviewing keeps its phase while its implementer merges the base forward for the implementing round of the drawn loop, 15 minutes (10:20 to 10:35), on ACME-2's slot.
  When the merge forward lands at 10:35, the recorded move `review_rejected` enters with no second read of that piece: ACME-2 moves to implementing at 10:35, testing 10:50, reviewing 11:05, accepting 11:20 (slot back), retro 11:35, merging 11:50.

- **12:05, ACME-2's merge** (recorded move at 11:50).
  The head that stands at 11:50 is `a9a3e0f7…`.
  Since the merge forward landed at 10:35, the replay's base took #11 at 11:15 alone, which does not meet `shelf/stock.go`: no touching merge.
  The store answers clean, n = 0, and ACME-2 merges at 12:05.

**The counts.**
Four reads of acme/widgets: 3 clean (ACME-1 at 10:15 and 11:15, ACME-2 at 12:05), 1 conflict (ACME-2 at 10:20), and no unanswered read.
The merge forward that the answered read began ran from 10:20 to 10:35: `gitLoopHours` is 0.25 answered and 0 drawn.

## The scorecard under `legion-v20.42.5`, worked by hand

The span is 4 hours, 240 minutes.

**Slots.**
ACME-1 09:00 to 10:30 (90 minutes), ACME-2 09:05 to 11:50 (165 minutes, held through both loops): 2 slots on 2001-03-05, waits 0 and 0, median 0; `firstSlot` ACME-1 09:00, ACME-2 09:05.

**Slot-hours.**
Every slot is held in a turn: 90 + 165 = 255 minutes, `slotHoursInTurn` 4.25 hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 11:15 (135 minutes), ACME-2 09:05 to 12:35 (210): 345 minutes, 5.75 hours, an average of 345/240 = 1.4375, and 2 at once from 09:05 to 11:15.

**Done and work left.**
ACME-1 at 11:15, ACME-2 at 12:35 (`done` 2); `workLeft` 0.

**Started work without a slot.**
Neither issue gives its slot back before a send-back: 0 in both readings.

**Past the review.**
ACME-1 10:30 to 11:15 (45 minutes) and ACME-2 11:50 to 12:35 (45), never at once, with no slot: `pastReviewNotHolding` 90 minutes, 1.5 hours, an average of 0.375, at most 1; `pastReviewHolding` 0.

**Priority taken.**
No issue waits for a slot: 0.

**Loops.**
`conflict`: ACME-2 from the move back at 10:20 to its return to reviewing at 10:50: 1 round, 0.5 hours.
`review_changes`: ACME-2 from the move back at 11:05 to its return to reviewing at 11:35: 1 round, 0.5 hours.

**Waits.**
None.

**Assumptions.**
1; 2 (past the review); 3 (send-back loops and conflicts); 9; 13 (moves back); 18 (the window records a conflict).
`assumptions` is [1, 2, 3, 9, 13, 18]; `scenario` is `none`.

## The scorecard under `main`, worked by hand

ACME-2's conflict keeps reviewing and takes a 15-minute merge forward (10:20 to 10:35) instead of the 45 minutes of the moved-back loop and re-entered review (10:20 to 11:05), so ACME-2 finishes 30 minutes sooner.

**Slots.**
ACME-1 09:00 to 10:30 (90 minutes), ACME-2 09:05 to 11:20 (135): 2 slots, waits 0 and 0, median 0; `firstSlot` as above.

**Slot-hours.**
225 minutes in a turn: `slotHoursInTurn` 3.75 hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 11:15 (135 minutes), ACME-2 09:05 to 12:05 (180): 315 minutes, 5.25 hours, an average of 315/240 = 1.3125, at most 2 (09:05 to 11:15).

**Done and work left.**
ACME-1 at 11:15, ACME-2 at 12:05 (`done` 2); `workLeft` 0.

**Started work without a slot.**
0 in both readings.

**Past the review.**
ACME-1 10:30 to 11:15 (45 minutes) and ACME-2 11:20 to 12:05 (45), never at once, with no slot: `pastReviewNotHolding` 90 minutes, 1.5 hours, an average of 0.375, at most 1; `pastReviewHolding` 0.

**Priority taken.**
0.

**Loops.**
`conflict`: ACME-2's merge forward 10:20 to 10:35, 1 round, 0.25 hours.
`review_changes`: ACME-2 from the move back at 10:35 to its return to reviewing at 11:05, 1 round, 0.5 hours.

**Waits.**
None.

**Assumptions.**
As above, plus 23 (merge forward): [1, 2, 3, 9, 13, 18, 23]; `scenario` is `none`.
