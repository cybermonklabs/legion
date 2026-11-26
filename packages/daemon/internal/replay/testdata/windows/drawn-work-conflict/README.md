# The window `drawn-work-conflict`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
five hours, from 2001-03-05T09:00Z to 14:00Z, an admission cap of 1, three issues, recorded under the rule `legion-v20.42.5` (a conflicting head moves back to implementing by `conflict`).
It holds one fact of check k1: work that the record never ran, drawn from a recorded issue that holds a conflict, keeps its source's recorded conflict on the main branch and draws it by its source's profile on the branch that draws, so it goes away where the drawn work's stretch takes no touching merge (spec R2).
`expect.json` uses `branches` to run `main` on two branches: `main+neverStarted=drawn` (the main branch with drawn work) and `main+conflicts=drawn+neverStarted=drawn` (the branch that draws conflicts too).
Each draw is certain, so every seed gives the same values.

## The issues, the base and git's reading

| issue | what it is | rank | priority | pull request |
| --- | --- | --- | --- | --- |
| ACME-1 | a root started first, whose review ends in a conflict at 10:15 | `a0` | 2 (P2) | acme/widgets#11 |
| ACME-2 | a root filed at 09:05, which takes the slot at 11:00 and designs to the window's end | `a1` | none | none |
| ACME-3 | a root filed at 09:10, which never starts in the record | `a2` | 2 (P2) | none |

| commit of `main` | pull request | `at` | paths |
| --- | --- | --- | --- |
| `bda92ac4…` | #20, of no issue of the window | 10:05 | `shelf/stock.go` |
| `1141fccb…` | #11, ACME-1's own | 11:15 | `shelf/stock.go` |
| `3cfafb9e…` | #30, of no issue of the window | 11:40 | `billing/invoice.go` |

| head of #11 | stands from | merge base | paths (the profile) | git's verdicts |
| --- | --- | --- | --- | --- |
| `5a1daa53…` | 09:45 | a commit before the window | `shelf/stock.go` | a conflict at #20's commit |
| `9f640d9e…` | 10:30 | #20's commit | `shelf/stock.go` | none |

## What happens (the record, under `legion-v20.42.5`)

`legion-v20.42.5` orders the line by rank alone, so ACME-2 takes the slot at 11:00 ahead of ACME-3, and ACME-3 never starts.

| time (UTC) | fact |
| --- | --- |
| 09:00 | the daemon boots; ACME-1 filed and promoted, slot 1 of 1; 15 minutes in each phase: planning 09:15, implementing 09:30, testing 09:45 (stretch 1 begins), reviewing 10:00 |
| 09:05, 09:10 | ACME-2, then ACME-3, filed: both wait |
| 10:05 | #20 lands on `main`, changing `shelf/stock.go` |
| 10:15 | ACME-1 `conflict`: reviewing to implementing, on its slot (stretch 1 ends in a conflict); turn to 10:30 |
| 10:30 | ACME-1 testing (stretch 2 begins); 10:45 reviewing (the loop is back in the phase it left) |
| 11:00 | ACME-1 accepting, its slot back; ACME-2 promoted in the same pass, its architect's turn open at 14:00 |
| 11:05, 11:10, 11:15 | ACME-1 retro (5 minutes), merging (5), merged (5): its merge lands `1141fccb…` at 11:15 |
| 11:40 | #30 lands on `main`, changing `billing/invoice.go` |

ACME-1's conflict loop is implementing 10:15 to 10:30 and testing 10:30 to 10:45; its implementing round is 15 minutes.
ACME-3 has no event after its filing.

## The fit

With git input held for acme/widgets, M counts each stretch's touching merges (`../README.md`, "The branch that asks git, and the generator's profile"):

- ACME-1's first stretch, 09:45 to 10:15, head `5a1daa53…`: #20 at 10:05 changes `shelf/stock.go`, M = 1, conflicted.

- ACME-1's second stretch, 10:30 to 11:15, head `9f640d9e…`: merge base is #20, no commit after it before 11:15, M = 0, left out of the fit.

The one stretch in the fit conflicted: a stretch with a touching merge draws a chance of 1, and a stretch with none draws a chance of 0.

## The replay under `main`, on `main+neverStarted=drawn`

Under `main` a conflicting head in reviewing keeps its phase while the implementer merges the base forward (15 minutes, the implementing round of ACME-1's loop), and the line orders by priority first.

- 09:00: ACME-1 is promoted and runs as recorded to 10:15.
  On the main branch every recorded conflict is kept as recorded, so ACME-1's conflict at 10:15 runs a merge forward from 10:15 to 10:30, then the reviewing segment after the loop from 10:30 to 10:45.

- 10:45: `review_approved`: ACME-1 reaches accepting and gives its slot back.
  ACME-1 finishes its five-minute accepting, retro and merging segments and merges at 11:00, where #11's commit enters the replay's base.

- 10:45: with ACME-2 (no priority) and ACME-3 (P2) waiting, ACME-3 takes the slot.
  ACME-3 never started in the record; its scenario `drawn` draws its work from the started issues of its priority and repository first, which is ACME-1 alone.
  Its cloned segments run from 10:45: planning 11:00, implementing 11:15, testing 11:30, reviewing 11:45.

- 12:00: the reviewing segment ends where ACME-1's record ended the stretch in a conflict.
  On the main branch drawn work keeps its source's recorded conflict, so ACME-3 conflicts at 12:00: its implementer merges the base forward for 15 minutes (12:00 to 12:15), and its reviewing segment after the loop runs 12:15 to 12:30.

- 12:30: `review_approved`: ACME-3 reaches accepting and gives its slot back; ACME-2 takes it in the same pass, its architect's turn open to 14:00.
  ACME-3 merges at 12:45.

## The replay under `main`, on `main+conflicts=drawn+neverStarted=drawn`

Up to 12:00 nothing differs: ACME-1's first stretch draws at 10:15 with M = 1 (#20 at 10:05 changes `shelf/stock.go`), chance 1, a conflict; its merge forward runs to 10:30, its slot goes to ACME-3 at 10:45, and ACME-1 merges at 11:00 (#11 enters the replay's base at 11:00).

- 12:00: ACME-3's drawn stretch ends, where ACME-1's record ended in a conflict, and draws from the generator.
  Work that the record never ran has no head of its own; it takes the profile of the issue that its scenario drew its work from (ACME-1's `shelf/stock.go`), and its touching merges count the commits that the replay's base took after the later of the replay's start of its first implementing segment (11:15) and the landing of its last merge forward (none).
  Between 11:15 and 12:00 the replay's base took #30 at 11:40 alone (#20 landed at 10:05 and #11 entered at ACME-1's replay merge at 11:00, both before 11:15).
  #30 changes `billing/invoice.go` alone, not `shelf/stock.go`: M = 0, chance 0, clean.
  The conflict goes away.
  ACME-3 goes on past the cloned loop: its reviewing segment runs 12:00 to 12:15.

- 12:15: `review_approved`: ACME-3 gives its slot back to ACME-2, whose architect's turn is open from 12:15 to 14:00.
  ACME-3 merges at 12:30 (the live stretch drew at 12:00 and draws no more).

A tool that counted every commit without reading profiles counts #30 at 11:40 in ACME-3's stretch (M = 1), draws a conflict at 12:00, and merges ACME-3 at 12:45 with two conflict rounds.

## The scorecard on `main+neverStarted=drawn`, worked by hand

The span is 5 hours, 300 minutes.

**Slots.**
ACME-1 09:00 to 10:45 (105 minutes), ACME-3 10:45 to 12:30 (105), ACME-2 12:30 to 14:00 (90): 3 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-3 10:45, ACME-2 12:30.
The waits run from each filing: ACME-1 0, ACME-3 from 09:10 to 10:45 (95), ACME-2 from 09:05 to 12:30 (205).
Sorted 0, 95, 205: the median is 95.

**Slot-hours.**
Every slot is held in a turn (each merge forward runs its implementer's turn while the issue holds its slot, and ACME-2's architect turn stays open to 14:00): 300 minutes, `slotHoursInTurn` 5 hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 11:00 (120 minutes), ACME-3 10:45 to 12:45 (120), ACME-2 12:30 to 14:00 (90): 330 minutes, 5.5 hours, an average of 330/300 = 1.1, and 2 at once from 10:45 to 11:00 and from 12:30 to 12:45.

**Done and work left.**
ACME-1 at 11:00, ACME-3 at 12:45 (`done` 2); ACME-2 is in flight at 14:00 (`workLeft` 1).

**Started work without a slot.**
0 in both readings.

**Past the review.**
ACME-1 10:45 to 11:00 (15 minutes) and ACME-3 12:30 to 12:45 (15), never at once, with no slot: `pastReviewNotHolding` 30 minutes, 0.5 hours, an average of 0.1, at most 1; `pastReviewHolding` 0.

**Priority taken.**
By rank (`priorityTaken`): from 09:05 to 10:45 ACME-1 (`a0`) holds above both waiters, 0; from 10:45 to 12:30 ACME-3 (`a2`) holds while ACME-2 (`a1`) waits, the one pair, taken: 105 minutes of a share of 1, 105/300 = 0.35; from 12:30 no issue waits.
By priority first (`priorityTakenBanded`): ACME-1 (P2, `a0`) holds above ACME-2 (none) and ACME-3 (P2, `a2`), and ACME-3 (P2) holds above ACME-2 (none): 0.

**Loops.**
Two rounds of `conflict`, ACME-1's merge forward 10:15 to 10:30 and ACME-3's 12:00 to 12:15: 2 rounds, 0.5 hours.

**Waits.**
None.

**Assumptions.**
1; 2 (past the review); 3 (conflicts); 8 (ACME-3 never started in the record); 9; 11 (a slot given out in the same pass at 10:45 and 12:30); 15 (ACME-2 still in flight at 14:00); 18 (the window records a conflict); 23 (merge forward).
Nothing moves back, so not 13.
`assumptions` is [1, 2, 3, 8, 9, 11, 15, 18, 23]; `scenario` is `drawn`.

## The scorecard on `main+conflicts=drawn+neverStarted=drawn`, worked by hand

ACME-3 has no merge forward, so its slot ends 15 minutes sooner, at 12:15, and ACME-2 starts 15 minutes sooner, at 12:15.
The measures that move:

- **Slots.**
  ACME-1 09:00 to 10:45 (105 minutes), ACME-3 10:45 to 12:15 (90), ACME-2 12:15 to 14:00 (105): 3 slots; `firstSlot` ACME-2 12:15; waits 0, 95 and 190, median 95.
  Slot-hours are still 300 minutes (5 hours).

- **In flight.**
  ACME-1 120 minutes, ACME-3 10:45 to 12:30 (105), ACME-2 12:15 to 14:00 (105): still 330 minutes, 5.5 hours, average 1.1, at most 2 (10:45 to 11:00 and 12:15 to 12:30).

- **Done.**
  ACME-1 at 11:00, ACME-3 at 12:30 (`done` 2, `workLeft` 1).

- **Priority taken.**
  ACME-3 holds while ACME-2 waits from 10:45 to 12:15, 90 minutes: `priorityTaken` 90/300 = 0.3; `priorityTakenBanded` 0.

- **Loops.**
  One round of `conflict`, ACME-1's merge forward 10:15 to 10:30: 1 round, 0.25 hours.

Every other measure is as on `main+neverStarted=drawn`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` on each branch that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
Each run limits all six signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `refusals`: no minute of the provider's record before 09:00.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted, and the drawn work's kills have no rate.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
