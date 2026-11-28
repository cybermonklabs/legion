# The window `git-unanswered-reads`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
four hours, from 2001-03-05T09:00Z to 13:00Z, an admission cap of 1, five issues, recorded under the rule `legion-v20.42.5`.
It holds four facts of check k1 on the branch that asks git (spec R2):
one unanswered read of each kind that the store cannot answer (`never-ran`, `after-merge`, `missing` and `order`), at each of which the store, read wrongly, holds a conflict verdict.
The window records no conflict, so its generator draws a chance of 0 and every unanswered read is clean: a tool that answers any of the four from the store makes a conflict and fails, and a tool that counts one under another kind fails the counts and lines.
`expect.json` holds the scorecard of a replay under `main` on the branch `main+conflicts=git+neverStarted=drawn`, with `gitReads`, `gitLoopHours` and each read's log line, worked by hand below.
No draw makes a conflict and the one drawn-work pool holds ACME-1 alone, so every seed gives the same values.

## The issues, the base and git's reading

| issue | what it is | rank | priority | pull request |
| --- | --- | --- | --- | --- |
| ACME-1 | a root started first, parked in accepting from 09:40 | `a0` | 2 (P2) | acme/widgets#11 |
| ACME-2 | a root started second in the record, merged at 10:35 | `a1` | none | acme/widgets#12 |
| ACME-3 | a root started third in the record, merged at 11:15 | `a2` | none | acme/widgets#13 |
| ACME-4 | a root started fourth in the record, implementing from 11:25 to 13:00 | `a3` | none | acme/widgets#14 |
| ACME-5 | a root filed at 09:04, never started in the record | `a4` | 2 (P2) | none |

| commit of `main` | pull request | `at` | `commit` line paths |
| --- | --- | --- | --- |
| `221c4651…` | #20, outside | 09:27 | `shelf/stock.go` |
| `699828ff…` | #21, outside | 10:07 | `aisle/sort.go` |
| `5da8541c…` | #12, ACME-2's own | 10:35 | none: a `missing` line (`timeout`, `pr` 0) names it, so its paths are unknown |
| `02c3804f…` | #22, outside | 10:40 | `lists/order.go` |
| `6dfc3c97…` | #23, outside | 10:47 | `pricing/tags.go` |
| `0da81a35…` | #13, ACME-3's own | 11:15 | `pricing/tags.go` |
| `3b93c89f…` | #24, outside | 11:17 | `tills/close.go` |

| head | stands from | merge base | paths | git's verdicts |
| --- | --- | --- | --- | --- |
| #11 `cd7d78f8…` | 09:15 | a commit before the window | `shelf/stock.go` | conflict at #20 |
| #11 `37370350…` | 09:30 | #20 | `shelf/stock.go` | none |
| #12 `5aaafba9…` | 09:55 | a commit before the window | `aisle/sort.go`, `lists/order.go` | conflict at #21 (#12 merged at 10:35, before #22) |
| #12 `700c16b7…` | 10:10 | #21 | `aisle/sort.go` | none |
| #13 `4c4ae4ef…` | 10:35 | #21 | `pricing/tags.go` | conflict at #23 (#12's commit has no `commit` line) |
| #13 `92981adb…` | 10:50 | #23 | `pricing/tags.go` | none |
| #14 `d9598937…` | 11:15 | #23 | `pricing/tags.go`, `tills/close.go` | conflict at #13, then conflict at #24 |

## What happens (the record, under `legion-v20.42.5`)

Every segment is five minutes, except ACME-1's open accepting segment (from 09:40) and ACME-4's open implementing segment (from 11:25).
`legion-v20.42.5` orders the line by rank alone, so ACME-1, ACME-2, ACME-3 and ACME-4 take the slot in turn, ACME-4 holds it to 13:00, and ACME-5 never starts.

| time (UTC) | fact |
| --- | --- |
| 09:00 | the daemon boots; ACME-1 filed and promoted, slot 1 of 1; 09:05 planning, 09:10 implementing, 09:15 testing, 09:20 reviewing |
| 09:01..09:04 | ACME-2 (09:01), ACME-3 (09:02), ACME-4 (09:03) and ACME-5 (09:04) filed: each waits |
| 09:25 | ACME-1 `review_rejected` to implementing; 09:30 testing, 09:35 reviewing, 09:40 accepting (slot back; architect turn open to 13:00) |
| 09:40 | ACME-2 promoted in the same pass: 09:45 planning, 09:50 implementing, 09:55 testing, 10:00 reviewing, 10:05 `review_rejected` |
| 10:10 | ACME-2 testing, 10:15 reviewing, 10:20 accepting (slot back); 10:25 retro, 10:30 merging, 10:35 merged (`5da8541c…`) |
| 10:20 | ACME-3 promoted in the same pass: 10:25 planning, 10:30 implementing, 10:35 testing, 10:40 reviewing, 10:45 `review_rejected` |
| 10:50 | ACME-3 testing, 10:55 reviewing, 11:00 accepting (slot back); 11:05 retro, 11:10 merging, 11:15 merged (`0da81a35…`) |
| 11:00 | ACME-4 promoted in the same pass: 11:05 planning, 11:10 implementing, 11:15 testing, 11:20 reviewing, 11:25 `review_rejected` (implementer turn open to 13:00) |

## The replay under `main`, on `main+conflicts=git+neverStarted=drawn`

`main` orders the line by priority first.
When ACME-1's slot comes back at 09:40, ACME-5 (P2) takes it ahead of ACME-2, ACME-3 and ACME-4 (none).
ACME-5 never started in the record, so `neverStarted=drawn` draws its work from the started issues of its priority and repository first, which is ACME-1 alone: 40 minutes on the slot, 09:40 to 10:20, then parked in accepting.
ACME-2, ACME-3 and ACME-4 therefore each start 40 minutes later than in the record: ACME-2 at 10:20 (merging at 11:15), ACME-3 at 11:00 (merging at 11:55) and ACME-4 at 11:40.
ACME-2's merge (`#12`, whose `commit` line is missing) enters the replay's base at 11:15 instead of 10:35, and ACME-3's merge (`#13`) enters at 11:55 instead of 11:15, after `#24` (11:17).

Every read below is read by the rules of `../README.md`, "The branch that asks git, and the generator's profile".
The window records no conflict, so the generator's chance is 0 for every unanswered read, and every read leaves the replay on the course above.

- **09:25, ACME-1** (`review_rejected`, recorded 09:25, head `cd7d78f8…`).
  The replay's base holds no commit yet (`#20` lands at 09:27): no touching merge, answered **clean**, n = 0.

- **10:05, ACME-5** (cloned `review_rejected`, recorded 09:25).
  ACME-5's work is drawn from ACME-1 and ACME-5 has no pull request in the record: unanswered, **`never-ran`**.
  A tool that read ACME-1's head `cd7d78f8…` from the store at 10:05 would see `#20` (09:27) in the replay's base and answer a conflict.

- **10:45, ACME-2** (`review_rejected`, recorded 10:05, head `5aaafba9…`).
  The replay's base holds `#20` (09:27, `shelf/stock.go`), `#21` (10:07, `aisle/sort.go`) and `#22` (10:40, `lists/order.go`); `#12` has not merged yet.
  Both `#21` and `#22` meet `5aaafba9…`'s paths (`aisle/sort.go`, `lists/order.go`), so the replay's touching merges are `[#21, #22]`.
  `#22` (10:40) landed after `#12`'s own recorded merge (10:35): unanswered, **`after-merge`**.
  A tool that answered from the store's verdict at `#21`, or by the longest matching prefix, would answer a conflict.

- **11:15, ACME-2** (`pull_request_merged`, recorded 10:35, head `700c16b7…`).
  `700c16b7…`'s merge base is `#21` and its path is `aisle/sort.go`.
  Since `#21` the replay's base took `#22` (`lists/order.go`) and `#23` (`pricing/tags.go`), neither meeting `aisle/sort.go`, and not yet `#12` itself: no touching merge, answered **clean**, n = 0.
  ACME-2 merges at 11:15, and `#12` (`5da8541c…`) enters the replay's base.

- **11:25, ACME-3** (`review_rejected`, recorded 10:45, head `4c4ae4ef…`).
  `4c4ae4ef…`'s merge base is `#21` (10:07).
  Since `#21` the replay's base holds `#22` (10:40), `#23` (10:47, `pricing/tags.go`), `#12` (entered at 11:15) and `#24` (11:17).
  `#12` has no `commit` line (a `missing` line names it), so its paths are unknown and the touching merges cannot be decided without it: unanswered, **`missing`**.
  A tool that skipped a commit without a `commit` line would see `[#23]` alone and answer a conflict from the store.

- **11:55, ACME-3** (`pull_request_merged`, recorded 11:15, head `92981adb…`).
  `92981adb…`'s merge base is `#23` (10:47), which comes after `#12` (10:35) on the record's first-parent chain, so the head already holds `#12`.
  The one commit of the replay's base after `#23` at 11:55 is `#24` (`tills/close.go`), which does not meet `pricing/tags.go`: no touching merge, answered **clean**, n = 0.
  ACME-3 merges at 11:55, and `#13` (`0da81a35…`) enters the replay's base.

- **12:05, ACME-4** (`review_rejected`, recorded 11:25, head `d9598937…`).
  `d9598937…`'s merge base is `#23` (10:47), so it already holds `#12`.
  Since `#23`, the replay's base took `#24` at 11:17 (`tills/close.go`) and then `#13` at 11:55 (`pricing/tags.go`): both meet `d9598937…`'s paths (`pricing/tags.go`, `tills/close.go`).
  The replay's touching merges are `[#24, #13]`, while the record's verdicts are `[#13, #24]`: the same commits in another order, neither after any merge of `#14` (which never merged).
  Not a prefix of the record's: unanswered, **`order`**.
  A tool that ignored merge order, or read the verdict of either commit alone, would answer a conflict from the store.

**The counts.**
Seven reads of acme/widgets: 3 clean, 0 conflict, 1 `order`, 1 `after-merge`, 1 `missing` and 1 `never-ran`.
No conflict loop runs: `gitLoopHours` is 0 answered and 0 drawn.

## The scorecard under `main`, worked by hand

The span is 4 hours, 240 minutes.

**Slots.**
ACME-1 09:00 to 09:40 (40 minutes), ACME-5 09:40 to 10:20 (40), ACME-2 10:20 to 11:00 (40), ACME-3 11:00 to 11:40 (40), ACME-4 11:40 to 13:00 (80): 5 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-5 09:40, ACME-2 10:20, ACME-3 11:00, ACME-4 11:40.
The waits run from each filing: ACME-1 0, ACME-5 from 09:04 to 09:40 (36), ACME-2 from 09:01 to 10:20 (79), ACME-3 from 09:02 to 11:00 (118), ACME-4 from 09:03 to 11:40 (157).
Sorted 0, 36, 79, 118, 157: the median is 79.

**Slot-hours.**
Every slot is held in a turn (ACME-4's implementer turn stays open from 12:05 to 13:00): 240 minutes, `slotHoursInTurn` 4 hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 13:00 (240 minutes), ACME-5 09:40 to 13:00 (200), ACME-2 10:20 to 11:15 (55), ACME-3 11:00 to 11:55 (55), ACME-4 11:40 to 13:00 (80):
630 minutes, 10.5 hours, an average of 630/240 = 2.625.
At 11:00 to 11:15 four issues are in flight (ACME-1, ACME-5, ACME-2, ACME-3), and again at 11:40 to 11:55 (ACME-1, ACME-5, ACME-3, ACME-4): at most 4.

**Done and work left.**
ACME-2 at 11:15, ACME-3 at 11:55 (`done` 2); ACME-1, ACME-5 and ACME-4 remain in flight at 13:00 (`workLeft` 3).

**Started work without a slot.**
0 in both readings.

**Past the review.**
ACME-1 09:40 to 13:00 (200 minutes), ACME-5 10:20 to 13:00 (160), ACME-2 11:00 to 11:15 (15), ACME-3 11:40 to 11:55 (15), none with a slot:
390 minutes, 6.5 hours, an average of 390/240 = 1.625, and 3 at once during 11:00 to 11:15 and 11:40 to 11:55; `pastReviewHolding` 0.

**Priority taken.**
By rank (`priorityTaken`):
from 09:01 to 09:40 ACME-1 (`a0`) holds above every waiter, 0;
from 09:40 to 10:20 ACME-5 (`a4`) holds while ACME-2 (`a1`), ACME-3 (`a2`) and ACME-4 (`a3`) wait, all three pairs taken: share 1 for 40 minutes;
from 10:20 to 11:00 ACME-2 (`a1`) holds above ACME-3 and ACME-4, 0;
from 11:00 to 11:40 ACME-3 (`a2`) holds above ACME-4, 0;
from 11:40 nobody waits.
40/240 = 0.166666….
By priority first (`priorityTakenBanded`): ACME-1 (P2, `a0`) holds above ACME-5 (P2, `a4`) and the three issues with none; ACME-5 (P2) holds above the three with none; then ACME-2 and ACME-3 hold in rank order: 0.

**Loops.**
Five rounds of `review_changes`: ACME-1 09:25 to 09:35 (10 minutes), ACME-5 10:05 to 10:15 (10), ACME-2 10:45 to 10:55 (10), ACME-3 11:25 to 11:35 (10), and ACME-4 from 12:05 cut at 13:00 (55):
95 minutes, 1.58333… hours.

**Waits.**
None.

**Assumptions.**
1; 2 (past the review); 3 (the review loops); 8 (ACME-5 never started in the record); 9; 11 (a slot given out in the same pass at 09:40, 10:20, 11:00 and 11:40); 13 (the review loops move back); 15 (three issues still in flight at 13:00).
`assumptions` is [1, 2, 3, 8, 9, 11, 13, 15]; `scenario` is `drawn`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all five signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
