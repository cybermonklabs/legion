# The window `review-and-test-kept`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: five hours,
from 2001-03-05T09:00Z to 14:00Z, an admission cap of 1, one issue, recorded under the rule `main`.
It holds one fact of check k1: a review that asks for changes and a failed test each keep their
recorded count and length (spec R2, assumption 3). ACME-1's test fails once and its review asks for
changes once, and the replay runs both loops as recorded, each with its cause. `expect.json` holds
the scorecard of a replay under `main` on the main branch, worked by hand below; nothing is drawn,
so every seed gives it.

## The issue

ACME-1, a root handed to Legion, rank `a0`, filed at 09:00.

## What happens

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots; ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 1, architect turn 09:00 to 09:20 |
| 09:20 | design registered | planning; turn 09:20 to 09:40 |
| 09:40 | `planner_completed` | implementing; turn 09:40 to 10:10 |
| 10:10 | `implementation_ready` | testing; turn 10:10 to 10:30 |
| 10:30 | `tester_failed` | back to implementing; ACME-1 holds its slot (implementing keeps one), so its implementer starts at once: turn 10:30 to 11:00 |
| 11:00 | `implementation_ready` | testing; turn 11:00 to 11:20 |
| 11:20 | `tester_passed` | reviewing; turn 11:20 to 11:40 |
| 11:40 | `review_rejected` | back to implementing, at once on its slot: turn 11:40 to 12:10 |
| 12:10 | `implementation_ready` | testing; turn 12:10 to 12:30 |
| 12:30 | `tester_passed` | reviewing; turn 12:30 to 12:50 |
| 12:50 | `review_approved` | accepting; its slot goes back; architect turn 12:50 to 13:00 |
| 13:00 | `sign_off` | retro; turn 13:00 to 13:10 |
| 13:10 | `retro_completed` | merging; turn 13:10 to 13:30 |
| 13:30 | `pull_request_merged` | done |

Every segment starts at the move into its phase (ACME-1 holds its slot from 09:00 to 12:50, and
past the review no slot is needed): 20, 20, 30, 20, 30, 20, 20, 30, 20, 20, 10, 10 and 20 minutes.

## The replay under `main`

The recorded rule. The failed test and the review's request for changes are recorded moves of
their segments' ends (`tester_failed`, `review_rejected`), each made at the end of its segment, and
the work after each keeps its recorded length: the replay runs both loops with their recorded
counts and lengths and is the record. A replay that drew these loops, or dropped them, would merge
ACME-1 at another time.

## The scorecard under `main`, worked by hand

The span is 5 hours.

**Slots.** ACME-1 from 09:00 to 12:50, 230 minutes: one slot on 2001-03-05, taken in the pass of its
filing (wait 0, median 0); `firstSlot` ACME-1 09:00.

**Slot-hours.** A turn runs from 09:00 to 13:30 with no gap (each move ends one turn and starts the
next), so all 230 minutes of the slot are in a turn: `slotHoursInTurn` 3.8333… hours,
`slotHoursNoTurn` 0.

**In flight.** 09:00 to 13:30, 4.5 hours, an average of 0.9, at most 1.

**Done and work left.** Done at 13:30 (`done` 1); `workLeft` 0.

**Started work without a slot.** ACME-1 holds its slot through both loops: 0 in both readings.

**Past the review.** Accepting, retro and merging from 12:50 to 13:30 with no slot:
`pastReviewNotHolding` 40 minutes, 0.6666… hours, an average of 0.13333…, at most 1.

**Priority taken.** No issue waits for a slot: 0.

**Loops.** The failed test (`test_failed`): one round, from the move back at 10:30 to ACME-1's next
entry into testing at 11:00, 0.5 hours. The review's changes (`review_changes`): one round, from the
move back at 11:40 to its next entry into reviewing at 12:30, 50 minutes, 0.8333… hours.

**Waits.** None.

**Assumptions.** 1; 2 (past the review); 3 (the loops keep their count and cause); 9; 13 (the loops
move ACME-1 back). `assumptions` is [1, 2, 3, 9, 13]; `scenario` is `none`.
