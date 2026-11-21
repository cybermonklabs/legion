# The window `reopen-works-again`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: five hours,
from 2001-03-05T09:00Z to 14:00Z, an admission cap of 1, one issue, recorded under the rule `main`.
It holds one fact of check k1: an issue that reopens keeps its later work (spec R2, assumption 16).
ACME-1 merges at 11:30, a person sets it back to `todo` at 12:00, and the daemon takes it in again
at its next generation and runs it until the window ends. `expect.json` holds the scorecard of a
replay under `main` on the main branch, worked by hand below; nothing is drawn, so every seed gives
it.

## The issue

ACME-1, a root handed to Legion, rank `a0`, filed at 09:00.

## What happens

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots; ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 1, architect turn 09:00 to 09:20 |
| 09:20 | design registered | planning; turn 09:20 to 09:40 |
| 09:40 | `planner_completed` | implementing; turn 09:40 to 10:10 |
| 10:10 | `implementation_ready` | testing; turn 10:10 to 10:30 |
| 10:30 | `tester_passed` | reviewing; turn 10:30 to 10:50 |
| 10:50 | `review_approved` | accepting; its slot goes back; architect turn 10:50 to 11:00 |
| 11:00 | `sign_off` | retro; turn 11:00 to 11:10 |
| 11:10 | `retro_completed` | merging; turn 11:10 to 11:30 |
| 11:30 | `pull_request_merged` | done; ACME-1 leaves the workflow and its tree lingers to 13:30 |
| 12:00 | a person sets ACME-1 back to `todo` (`events/ACME-1.jsonl`, seq 7) | admission takes the lingering root in again at generation 2 (`readmit`): a root whose new gate never opened, so it waits in the line as a root that never ran, and the free slot is its at once: slot 1 of 1, its architect launched for generation 2, turn 12:00 to 12:20 |
| 12:20 | the architect registers version 2 of the design (gate open) | planning by `gate_opened`; planner turn 12:20 to 12:50 |
| 12:50 | `planner_completed` | implementing; turn 12:50 to 13:40 |
| 13:40 | `implementation_ready` | testing; the tester's turn 13:40 is open at 14:00 |

The re-admission ends the linger before its 13:30 close, so no `workflow: linger closed the tree`
line follows. The work segments of generation 1: 20, 20, 30, 20, 20, 10, 10, 20 minutes; of
generation 2: admitted 12:00 to 12:20 (20), planning 12:20 to 12:50 (30), implementing 12:50 to
13:40 (50), testing from 13:40, unended at 14:00.

## The replay under `main`

The recorded rule. The person's `todo` at 12:00 is an event at its recorded time, and generation
2's segments enter at their recorded offsets from its start at 12:00, so the replay is the record.
A replay that dropped the reopened issue's later work (assumption 16's fault of the first replay)
would hold no slot after 11:30.

## The scorecard under `main`, worked by hand

The span is 5 hours.

**Slots.** ACME-1 from 09:00 to 10:50 (110 minutes) and from 12:00 to 14:00 (120): 2 slots on
2001-03-05, each taken in the pass where ACME-1 began to wait for it (its filing; its reopening),
so the waits are 0 and 0 and the median 0. `firstSlot` is ACME-1's first in the span, 09:00.

**Slot-hours.** Both slots are held in a turn throughout: 09:00 to 10:50, five turns back to back
(110 minutes); 12:00 to 14:00, architect, planner, implementer and tester back to back (120). 230
minutes: `slotHoursInTurn` 3.8333… hours, `slotHoursNoTurn` 0.

**In flight.** 09:00 to 11:30 (150 minutes) and again from its next start at 12:00 to 14:00 (120):
270 minutes, 4.5 hours, an average of 0.9, at most 1.

**Done and work left.** ACME-1 reaches `done` at 11:30 (`done` 1, `doneAt` 11:30), and is in flight
at 14:00 (`workLeft` 1): the two count different things, and the reopened issue is in both.

**Started work without a slot.** None: 0 in both readings.

**Past the review.** 10:50 to 11:30 with no slot: `pastReviewNotHolding` 40 minutes, 0.6666…
hours, an average of 0.13333…, at most 1.

**Priority taken.** No issue waits for a slot: 0.

**Loops and waits.** None.

**Assumptions.** 1; 2 (past the review); 9; 15 (ACME-1 in flight at 14:00); 16 (ACME-1 reopens).
`assumptions` is [1, 2, 9, 15, 16]; `scenario` is `none`.
