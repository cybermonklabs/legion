# The window `merge-event-time`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
four hours, from 2001-03-05T09:00Z to 13:00Z, an admission cap of 1, one issue, recorded under the rule `main`.
It holds one fact of check k1: a merge into the base whose committer's time and GitHub merge event differ lands in the replay's base at the merge event (spec R2).
`merges/acme__widgets.jsonl` holds both times of each commit, `at` and `committed`, and `git/acme__widgets.jsonl` holds git's verdicts of ACME-1's pull request.
`expect.json` holds the scorecard of a replay under `main` on the branch `main+conflicts=git`, with the run's counts of reads and the line of each read, worked by hand below.
The window records no conflict, so no draw decides anything and every seed gives the same values.

## The issue and the base

ACME-1, a root handed to Legion, rank `a0`, filed at 09:00, with the pull request acme/widgets#11.

| commit | pull request | `committed` | `at` (GitHub's merge event) | paths |
| --- | --- | --- | --- | --- |
| `86e878e2…` | #20, of no issue of the window | 10:00 | 10:30 | `shelf/stock.go` |
| `2f99d18a…` | #11, ACME-1's own | 11:45 | 11:45 | `shelf/stock.go` |

A merge queue built #20's commit at 10:00 and GitHub merged it at 10:30: `main` holds it from 10:30.

| head of #11 | stands from | merge base | paths | git's verdicts |
| --- | --- | --- | --- | --- |
| `b0904d26…` | 09:45 | a commit before the window | `shelf/stock.go` | a conflict at #20's commit |
| `55bcda55…` | 10:30 | #20's commit | `shelf/stock.go` | none: no commit of the base after its merge base and before its own merge |

## What happens (the record, under `main`)

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots; ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 1, architect turn 09:00 to 09:15 |
| 09:15 | design registered | planning; turn to 09:30 |
| 09:30 | `planner_completed` | implementing; turn to 09:45 |
| 09:45 | `implementation_ready` | testing (stretch 1 begins); turn to 10:00 |
| 10:00 | `tester_passed` | reviewing; turn to 10:15 |
| 10:15 | `review_rejected` | back to implementing on its slot (stretch 1 ends); turn to 10:30 |
| 10:30 | `implementation_ready` | testing (stretch 2 begins); turn to 10:45 |
| 10:45 | `tester_passed` | reviewing; turn to 11:00 |
| 11:00 | `review_approved` | accepting; its slot goes back; architect turn to 11:15 |
| 11:15 | `sign_off` | retro; turn to 11:30 |
| 11:30 | `retro_completed` | merging; turn to 11:45 |
| 11:45 | `pull_request_merged` | done (stretch 2 ends); its merge lands `2f99d18a…` |

Every segment is 15 minutes and starts at the move into its phase.

## The replay under `main`, on `main+conflicts=git`

The recorded rule, so every segment starts at its recorded time and the replay is the record, as long as no read finds a conflict.
The replay reads ACME-1's merge state at the end of each stretch (`../README.md`, "The branch that asks git").

**The read at 10:15.**
The move that ends stretch 1 is recorded at 10:15, and the head that stands then is `b0904d26…`, from 09:45.
The replay's base holds a commit from its `at` on: #20's commit lands at 10:30, so at 10:15 the base holds no commit after the head's merge base.
The head's touching merges are none, the first 0 of the one git's verdicts name: the store answers clean, n = 0.
The recorded move is no conflict, so nothing changes, and `review_rejected` enters at 10:15.
A tool that places #20's commit at its `committed`, 10:00, finds it in the base at 10:15: it changes `shelf/stock.go`, which the head changes, it is the first of the record's touching merges, and git's verdict at it is a conflict.
That tool makes a conflict at 10:15, asks the implementer to merge the base forward, and, since the window records no conflict loop to take the length of that work from, leaves ACME-1 in reviewing to the window's end: `done` 0.

**The read at 11:45.**
The move that ends stretch 2, the merge, is recorded at 11:45, and the head that stands then is `55bcda55…`, from 10:30.
Its merge base is #20's commit, so the base holds no commit after it at 11:45 but ACME-1's own merge, which enters the replay's base only as the replay merges ACME-1, after the read.
The store answers clean, n = 0, and ACME-1 merges at 11:45.

**The counts.**
Two reads of acme/widgets, both answered clean: `gitReads` is 2 clean, no conflict and no unanswered read of any kind.
No read began a conflict loop and the generator began none: `gitLoopHours` is 0 answered and 0 drawn.

## The scorecard under `main`, worked by hand

The span is 4 hours.

**Slots.**
ACME-1 from 09:00 to 11:00, 120 minutes: one slot on 2001-03-05, taken in the pass of its filing (wait 0, median 0); `firstSlot` ACME-1 09:00.

**Slot-hours.**
A turn runs from 09:00 to 11:00 with no gap, so the slot's 120 minutes are in a turn: `slotHoursInTurn` 2 hours, `slotHoursNoTurn` 0.

**In flight.**
09:00 to 11:45, 165 minutes, 2.75 hours, an average of 2.75/4 = 0.6875, at most 1.

**Done and work left.**
Done at 11:45 (`done` 1); `workLeft` 0.

**Started work without a slot.**
ACME-1 holds its slot through the review's loop: 0 in both readings.

**Past the review.**
Accepting, retro and merging from 11:00 to 11:45 with no slot: `pastReviewNotHolding` 45 minutes, 0.75 hours, an average of 0.1875, at most 1; `pastReviewHolding` 0.

**Priority taken.**
No issue waits for a slot: 0.

**Loops.**
The review's changes (`review_changes`): one round, from the move back at 10:15 to ACME-1's next entry into reviewing at 10:45, 0.5 hours.

**Waits.**
None.

**Assumptions.**
1; 2 (past the review); 3 (the loop keeps its count and cause); 9; 13 (the loop moves ACME-1 back).
`assumptions` is [1, 2, 3, 9, 13]; `scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all six signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `refusals`: no minute of the provider's record before 09:00.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
