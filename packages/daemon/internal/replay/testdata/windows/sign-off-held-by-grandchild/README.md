# The window `sign-off-held-by-grandchild`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
three hours, from 2001-03-05T09:00Z to 12:00Z, an admission cap of 2, a root, a child and a grandchild, recorded under the rule `main`.
It holds a sign-off the workflow holds behind an issue beneath the root that is no Dispatch child of it: ACME-3, the child of ACME-1's child ACME-2, which a person moved to icebox before ACME-1 started.
So no open child holds ACME-1's next start (`record.LineFacts.StartedParentWaits` reads its Dispatch children, and ACME-2 in icebox is not open), and ACME-1 reaches accepting under every rule, while ACME-3, a member of its tree in the workflow, holds its sign-off (`SIGNOFF_CHILD_IN_WORKFLOW`, `record.Beneath`).
`TestAHeldSignOffIsAWaitACoverHolds` (`internal/replay`) replays it under `cover`, where the held sign-off is a wait a cover holds.
`expect.json` holds the scorecards of replays under `main` and under `cover` on the main branch, worked by hand below; nothing is drawn, so every seed gives them.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | the root, handed to Legion | `a0` |
| ACME-2 | a child of ACME-1, filed `todo` by a person at 09:10 and moved to icebox at 09:15 | `a1` |
| ACME-3 | a child of ACME-2 in ACME-1's tree, filed `todo` by a person at 09:12 | `a2` |

## What happens

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots | |
| 09:05 | ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 2, architect turn 09:05 to 09:20 |
| 09:10 | ACME-2 filed under ACME-1, `todo` | recorded at admitted in ACME-1's tree; the gate has no row, so the line holds it |
| 09:12 | ACME-3 filed under ACME-2, `todo` | recorded at admitted in ACME-1's tree; the line holds it the same way |
| 09:15 | a person moves ACME-2 to icebox | ACME-2 leaves the workflow, never started |
| 09:20 | ACME-1's design registered (gate open) | ACME-1 planning by `gate_opened`, no open child under it; ACME-3's first phase starts in the same pass: slot 2 of 2, planning, planner turn 09:20 to 09:50 |
| 09:40 | ACME-1 `planner_completed` | implementing; turn 09:40 to 10:00 |
| 09:50 | ACME-3 `planner_completed` | implementing; turn 09:50 to 10:20 |
| 10:00 | ACME-1 `implementation_ready` | testing; turn 10:00 to 10:10 |
| 10:10 | ACME-1 `tester_passed` | reviewing; turn 10:10 to 10:20 |
| 10:20 | ACME-1 `review_approved`; ACME-3 `implementation_ready` | ACME-1 accepting, its slot back (1 of 2), architect turn 10:20 to 10:30; ACME-3 testing, turn 10:20 to 10:40 |
| 10:30 | ACME-1's architect ends its turn and signs off | the sign-off is refused: ACME-3 is in the workflow beneath ACME-1 (`SIGNOFF_CHILD_IN_WORKFLOW`), and the log holds no line of it |
| 10:40 | ACME-3 `tester_passed` | reviewing; turn 10:40 to 11:00 |
| 11:00 | ACME-3 `review_approved` | accepting; its slot back (0 of 2); architect turn 11:00 to 11:10 |
| 11:10 | ACME-3 `sign_off` | retro; turn 11:10 to 11:20 |
| 11:20 | ACME-3 `retro_completed` | merging; turn 11:20 to 11:30 |
| 11:30 | ACME-3 `pull_request_merged` | ACME-3 done; at its `workflow: issue left the workflow` the architect's sign-off of ACME-1 is applied again and holds: ACME-1 to retro by `sign_off`, implementer turn 11:30 to 11:40 |
| 11:40 | ACME-1 `retro_completed` | merging; merger turn 11:40 to 11:50 |
| 11:50 | ACME-1 `pull_request_merged` | ACME-1 done; its tree lingers to 13:50 |
| 12:00 | the window ends | |

ACME-1's segments: admitted 09:05 to 09:20 (15 minutes), planning 20, implementing 20, testing 10, reviewing 10, and its accepting segment, which ends at its architect's turn at 10:30: a held sign-off by `../README.md`, whose move comes at 11:30, at ACME-3's leave; retro 10 and merging 10.
ACME-3's: planning 30, implementing 30, testing 20, reviewing 20, accepting 10, retro 10 and merging 10 minutes.

## The replay under `main`

The recorded rule, today's: no open Dispatch child stands under ACME-1 once it starts, so #4592's wait never moves it.
Each recorded move enters at the end of its segment; ACME-1's sign-off is applied at 10:30, refused while ACME-3 is in the workflow, and applied again at ACME-3's leave at 11:30, when it holds (`divergence.heldSignOffs` 1).
Every pass falls at its recorded time and every segment starts at its recorded worker start with n = m, so the replay is the record.

## The scorecard under `main`, worked by hand

The span is 3 hours.

**Slots.**
ACME-1 09:05 to 10:20 (75 minutes) and ACME-3 09:20 to 11:00 (100): 2 slots on 2001-03-05 (`slotsGivenByDay`); `firstSlot` ACME-1 09:05, ACME-3 09:20.
ACME-1 took its slot at its filing and ACME-3 at the gate's opening, the pass where its first phase could start: waits 0 and 0, median 0.

**Slot-hours.**
ACME-1: architect 09:05 to 09:20, planner, implementer, tester and reviewer 09:20 to 10:20, 75 minutes in a turn.
ACME-3: planner, implementer, tester and reviewer 09:20 to 11:00, 100 minutes in a turn.
`slotHoursInTurn` is 175 minutes, 2.91666… hours; `slotHoursNoTurn` is 0.

**In flight.**
ACME-1 from 09:05 to 11:50 (165 minutes) and ACME-3 from 09:20 to 11:30 (130); ACME-2 never starts:
295 minutes, 4.91666… hours, an average of 4.91666…/3 = 1.63888…, at most 2 (`inFlight`).

**Done and work left.**
ACME-3 done at 11:30 and ACME-1 at 11:50 (`done` 2, `doneAt`); `workLeft` 0.

**Started work without a slot.**
None: no issue ever stands in the line's returned part or in `awaiting_children`.
Both readings are 0.

**Past the review.**
ACME-1 in accepting, retro and merging from 10:20 to 11:50 (90 minutes) and ACME-3 from 11:00 to 11:30 (30), neither with a slot:
`pastReviewNotHolding` 2 hours, an average of 2/3 = 0.66666…, at most 2 (11:00 to 11:30); `pastReviewHolding` 0.

**Priority taken.**
No issue ever waits for a slot: `priorityTaken` 0.

**Loops.**
None.

**Waits on another issue.**
None: no issue is linked, no children wait holds a first phase (ACME-2 is in icebox when ACME-1's gate opens), and the held sign-off stands past ACME-1's review, where the measures count no wait.

**Assumptions.**
1; 2 (ACME-1 and ACME-3 past their review); 7 (the gate holds ACME-3's first start from 09:12 to 09:20); 8 (ACME-2's work, never started, is unknown); 9 (slots given out); 10 (children); 17 (both children filed after ACME-1 started).
No slot is freed for a waiting issue, so 11 is not named.
`assumptions` is [1, 2, 7, 8, 9, 10, 17]; `scenario` is `none`.

## The replay under `cover`

Spec v4's rule: a started issue keeps its slot in every phase, and an issue a counted waiter waits on starts at once on that waiter's slot, which it holds for the waiter while the cover stands (`record.TakeCover`).
To 10:30 it is the record's course, but ACME-1 keeps its slot in accepting.
At 10:30 the workflow holds ACME-1's sign-off behind ACME-3: in accepting the issues beneath it that its close waits for are a wait a cover reads (`record.LineFacts.BeneathWaits`, `blockers.CoverWaits`), and ACME-3 holds a slot and covers nobody, so ACME-1's slot goes back and ACME-3 holds it for ACME-1.
At 11:30 ACME-3 is done: the cover ends, ACME-1 takes its slot again in the same pass (`admission gives a slot back to an issue whose cover ended`), and its sign-off is applied again and holds.
ACME-3 keeps its slot through accepting, retro and merging to its leave at 11:30; ACME-1 keeps its slot through retro and merging to its leave at 11:50.

## The scorecard under `cover`, worked by hand

The span is 3 hours.

**Slots.**
ACME-1 09:05 to 10:30 (85 minutes) and 11:30 to 11:50 (20), ACME-3 09:20 to 11:30 (130): 3 slots on 2001-03-05; `firstSlot` ACME-1 09:05, ACME-3 09:20; each taken in the pass that owed it, median 0.

**Slot-hours.**
Every slot is held in a turn throughout, the architects' turns in accepting and the retro and merge turns among them: 85 + 20 + 130 = 235 minutes, `slotHoursInTurn` 3.91666… hours, `slotHoursNoTurn` 0.

**In flight.**
As under `main`: 295 minutes, 4.91666… hours, an average of 1.63888…, at most 2.

**Done and work left.**
As under `main`: `done` 2, ACME-3 at 11:30 and ACME-1 at 11:50; `workLeft` 0.

**Started work without a slot.**
ACME-1, a covered waiter from 10:30 to 11:30 (60 minutes): reading (b) 1 hour, an average of 1/3 = 0.33333…, at most 1; reading (a) 0, since it waits on ACME-3.

**Past the review.**
Holding a slot: ACME-1 10:20 to 10:30 (10 minutes) and 11:30 to 11:50 (20), ACME-3 11:00 to 11:30 (30), never two at once: `pastReviewHolding` 1 hour, an average of 0.33333…, at most 1.
Holding none: ACME-1 10:30 to 11:30, covered: `pastReviewNotHolding` 1 hour, an average of 0.33333…, at most 1.

**Priority taken.**
No issue ever waits for a slot: 0.

**Loops.**
None.

**Waits on another issue.**
None: the held sign-off stands past ACME-1's review, where the measures count no wait; it is the cover's, which reading (b) counts.

**Assumptions.**
Those of `main`, and 11 (the slot ACME-3 frees at its leave at 11:30, to ACME-1 in the same pass) and 14 (the cover of ACME-3 for ACME-1).
`assumptions` is [1, 2, 7, 8, 9, 10, 11, 14, 17]; `scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all five signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
