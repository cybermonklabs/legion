# The window `every-trigger`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
seven hours, from 2001-03-05T09:00Z to 16:00Z, an admission cap of 2, a root and two children, recorded under the rule `legion-v22.14.1`.
It takes a root and two children through every trigger the workflow has: a failed test, a review that asks for changes, an architect's send-back from accepting, a sign-off held behind a child, the merge and the linger's expiry.
The record is of a daemon before cybermonklabs/legion#4592, so its part names `legion-v22.14.1`, the rule of release 22.14.1, as the rule it was made under: it was written under the `main` of its time, whose slot choices that release keeps.
Today's `main` moves a started parent with an open Dispatch child into `awaiting_children`: under it ACME-1 waits there for ACME-3, so its sign-off comes after ACME-3's leave and holds at once, and the held sign-off is replayed in the window `sign-off-held-by-grandchild`.
A replay under the recorded rule must give each issue the recorded sequence of (from, to, trigger), which `expect.json` holds under `moves` beside the scorecards, with no move refused; a replay under today's `main` gives the same sequences, at later times for ACME-1.
`expect.json`'s scorecards are of replays under `main`, under `legion-v22.14.1` and under `cover`, on the main branch, worked by hand below; nothing is drawn, so every seed gives them.
The record's own scorecard is the one under `legion-v22.14.1` (`TestScorecardOfTheRecord`).

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | the root | `a0` |
| ACME-2 | a child of ACME-1, filed `todo` by ACME-1's architect at 09:10 | `a1` |
| ACME-3 | a child of ACME-1, filed `todo` by ACME-1's planner at 11:40 | `a2` |

## What happens

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots; ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 2, architect turn 09:00 to 09:20 |
| 09:10 | ACME-2 filed under ACME-1 by its architect, `todo` | recorded at admitted in ACME-1's tree; the gate has no row, so the line holds it |
| 09:20 | ACME-1's architect ends its turn and registers its design (gate open) | ACME-1 at admitted under an open gate with an open child: its children wait begins (`admission: an issue waits for the Dispatch children under it`) and its slot goes back; in the same pass ACME-2's first phase starts: slot 1 of 2, planning by `gate_opened`, its architect launched, planner turn 09:20 to 09:30 |
| 09:30 | ACME-2 `planner_completed` | implementing; turn 09:30 to 09:40 |
| 09:40 | ACME-2 `implementation_ready` | testing; turn 09:40 to 09:50 |
| 09:50 | ACME-2 `tester_failed` | back to implementing, on its slot: turn 09:50 to 10:00 |
| 10:00 | ACME-2 `implementation_ready` | testing; turn 10:00 to 10:10 |
| 10:10 | ACME-2 `tester_passed` | reviewing; turn 10:10 to 10:20 |
| 10:20 | ACME-2 `review_approved` | accepting; its slot goes back (0 of 2); its architect's turn 10:20 to 10:30 |
| 10:30 | ACME-2's architect sends it back to implementing (`backward`) | the start is owed with `needsSlot`; a slot is free, so ACME-2 takes it in the same pass (`admission gives a slot back to an issue whose work came back`, implementing): turn 10:30 to 10:40 |
| 10:40 | ACME-2 `implementation_ready` | testing; turn 10:40 to 10:50 |
| 10:50 | ACME-2 `tester_passed` | reviewing; turn 10:50 to 11:00 |
| 11:00 | ACME-2 `review_approved` | accepting; slot back; architect turn 11:00 to 11:10 |
| 11:10 | ACME-2 `sign_off` | retro; turn 11:10 to 11:20 |
| 11:20 | ACME-2 `retro_completed` | merging; merger turn 11:20 to 11:30 |
| 11:30 | ACME-2 `pull_request_merged` | ACME-2 done, out of the workflow; with its `done` ACME-1's children wait ends: `admission gives a slot back to an issue whose wait ended` (admitted), slot 1 of 2, and its own first phase starts in the same pass: planner turn 11:30 to 11:50 |
| 11:40 | ACME-3 filed under ACME-1 by its planner, `todo` | recorded at admitted; the gate is open and ACME-1 stands in planning (no children wait), so ACME-3's first phase starts at once: slot 2 of 2, planning, its architect launched, planner turn 11:40 to 11:50 |
| 11:50 | ACME-1 and ACME-3 `planner_completed` | implementing, turns 11:50 to 12:00 |
| 12:00 | ACME-1 and ACME-3 `implementation_ready` | testing, turns 12:00 to 12:10 |
| 12:10 | ACME-1 and ACME-3 `tester_passed` | reviewing, turns 12:10 to 12:20 |
| 12:20 | ACME-1 `review_approved`; ACME-3 `review_rejected` | ACME-1 accepting, slot back, architect turn 12:20 to 12:30; ACME-3 back to implementing on its slot, turn 12:20 to 12:30 |
| 12:30 | ACME-1's architect ends its turn and signs off; ACME-3 `implementation_ready` | the sign-off is refused: ACME-3 is in the workflow beneath ACME-1 (`SIGNOFF_CHILD_IN_WORKFLOW`), and the log holds no line of it; ACME-3 testing, turn 12:30 to 12:40 |
| 12:40 | ACME-3 `tester_passed` | reviewing; turn 12:40 to 12:50 |
| 12:50 | ACME-3 `review_approved` | accepting; slot back; architect turn 12:50 to 13:00 |
| 13:00 | ACME-3 `sign_off` | retro; turn 13:00 to 13:10 |
| 13:10 | ACME-3 `retro_completed` | merging; turn 13:10 to 13:20 |
| 13:20 | ACME-3 `pull_request_merged` | ACME-3 done; at its `workflow: issue left the workflow` the architect's sign-off of ACME-1 is applied again and holds: ACME-1 to retro by `sign_off`, implementer turn 13:20 to 13:30 |
| 13:30 | ACME-1 `retro_completed` | merging; merger turn 13:30 to 13:40 |
| 13:40 | ACME-1 `pull_request_merged` | ACME-1 done; its tree lingers to 15:40 |
| 15:40 | the linger expires | `workflow: linger closed the tree`, 3 issues |

Every segment is 10 minutes but ACME-1's admitted (09:00 to 09:20) and planning (11:30 to 11:50),
20 minutes each, and its accepting segment, which ends at its architect's turn at 12:30: a held
sign-off by `../README.md`, whose move comes at 13:20, at ACME-3's leave.

## The replay under `legion-v22.14.1`

The rule of release 22.14.1, before #4592: no open child stops a started parent's phase.
Each recorded move enters at the end of its segment (`../README.md`, "Work segments and their replayed length"):
the handoff moves through `ReplayTransition`, which refuses unless the issue stands where the record says;
the sign-offs through `intake.SignOff`; the merges through `intake.PullRequestMerged` on the replay's own pull request record;
the send-back through `intake.BackwardMove`; and `gate_opened` is the pass's own promotion, never injected.
ACME-1's sign-off is applied at 12:30, refused while ACME-3 is in the workflow, and applied again at ACME-3's leave at 13:20, when it holds;
the linger's close comes from the replay's own `linger_close` row at its `next_at`, 15:40.
ACME-2's filing comes 10 minutes into ACME-1's first segment and ACME-3's 10 minutes into its planning segment, each at its recorded time since those segments start at their recorded times.
So every move falls at its recorded time with no refusal but the sign-off's hold, and each issue's sequence is the record's:

- ACME-1: admitted→planning `gate_opened`, planning→implementing `planner_completed`,
  implementing→testing `implementation_ready`, testing→reviewing `tester_passed`,
  reviewing→accepting `review_approved`, accepting→retro `sign_off`, retro→merging
  `retro_completed`, merging→done `pull_request_merged`.

- ACME-2: admitted→planning `gate_opened`, planning→implementing `planner_completed`,
  implementing→testing `implementation_ready`, testing→implementing `tester_failed`,
  implementing→testing `implementation_ready`, testing→reviewing `tester_passed`,
  reviewing→accepting `review_approved`, accepting→implementing `backward`, implementing→testing
  `implementation_ready`, testing→reviewing `tester_passed`, reviewing→accepting `review_approved`,
  accepting→retro `sign_off`, retro→merging `retro_completed`, merging→done `pull_request_merged`.

- ACME-3: admitted→planning `gate_opened`, planning→implementing `planner_completed`,
  implementing→testing `implementation_ready`, testing→reviewing `tester_passed`,
  reviewing→implementing `review_rejected`, implementing→testing `implementation_ready`,
  testing→reviewing `tester_passed`, reviewing→accepting `review_approved`, accepting→retro
  `sign_off`, retro→merging `retro_completed`, merging→done `pull_request_merged`.

The linger's expiry moves no phase: it closes a tree whose three issues are done, and no measure
reads it.

## The scorecard under `legion-v22.14.1`, worked by hand

The span is 7 hours.
The record was made under this rule, and `legion-v22.14.1` differs from the `main` of its time in two choices only: the start of a P0 issue and the order of the line.
The window holds no P0 issue, and no two issues ever wait in the line at once, so neither choice moves anything, and every value below is the record's.

**Slots.**
ACME-1 09:00 to 09:20 (20 minutes) and 11:30 to 12:20 (50); ACME-2 09:20 to 10:20 (60) and 10:30 to 11:00 (30); ACME-3 11:40 to 12:50 (70).
5 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 09:20, ACME-3 11:40.
Each was taken in the pass where its issue began to wait for it (ACME-1's filing;
ACME-2's first phase at the gate's opening, before which the gate held it;
ACME-2's send-back with a slot free; the end of ACME-1's children wait; ACME-3's filing under an open gate): all 0, median 0.

**Slot-hours.**
Every slot is held in a turn throughout:
ACME-1's architect 09:00 to 09:20; ACME-2's six turns 09:20 to 10:20 and three 10:30 to 11:00;
ACME-1's planner, implementer, tester and reviewer 11:30 to 12:20; ACME-3's seven turns 11:40 to 12:50.
20 + 60 + 30 + 50 + 70 = 230 minutes: `slotHoursInTurn` 3.8333… hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 13:40 (280 minutes), ACME-2 09:20 to 11:30 (130), ACME-3 11:40 to 13:20 (100): 510 minutes, 8.5 hours, an average of 1.214285…, at most 2.

**Done and work left.**
ACME-2 11:30, ACME-3 13:20, ACME-1 13:40 (`done` 3); `workLeft` 0.

**Started work without a slot.**
ACME-1, given back for its children wait, from 09:20 to 11:30: reading (b) 130 minutes, 2.1666… hours, an average of 0.309523…, at most 1.
It is in `admission.blocked` all that time and takes its slot in the pass that ends the wait: reading (a) 0.
ACME-2's send-back took its slot in the pass that owed it.

**Past the review.**
ACME-2 10:20 to 10:30 (10 minutes) and 11:00 to 11:30 (30), ACME-1 12:20 to 13:40 (80), ACME-3 12:50 to 13:20 (30), none with a slot:
`pastReviewNotHolding` 150 minutes, 2.5 hours, an average of 0.357142…, and 2 at once from 12:50 to 13:20; `pastReviewHolding` 0.

**Priority taken.**
No issue waits for a slot: 0.

**Loops.**
`test_failed`: ACME-2, from 09:50 back to testing at 10:00, 10 minutes (0.1666… hours).
`send_back`: ACME-2, from 10:30 back to accepting at 11:00, 30 minutes (0.5 hours).
`review_changes`: ACME-3, from 12:20 back to reviewing at 12:40, 20 minutes (0.3333… hours).
One round each.

**Waits on another issue.**
ACME-1's children wait, 09:20 to 11:30, ended by its last open child's `done`: `child_done`, count 1, 2.1666… hours.
ACME-3 is filed while ACME-1 stands in planning, where no children wait holds it under this rule (`record.SlotRule.ChildrenFirstCandidates` reads an issue at admitted, and the rule's wait stops no phase).

**Assumptions.**
1; 2; 3 (three loops keep their count and cause);
4 (ACME-1's children wait); 7 (the gate holds ACME-2's first start from 09:10 to 09:20);
9; 10 (children); 11 (ACME-1's slot, freed at 09:20, to ACME-2); 13 (the loops move issues back); 17 (both children filed after ACME-1 started).
`assumptions` is [1, 2, 3, 4, 7, 9, 10, 11, 13, 17]; `scenario` is `none`.

## The replay under `main`

Today's rule, with cybermonklabs/legion#4592's wait.
To 11:30 it is the record: ACME-1's children wait at admitted, ACME-2's course, and ACME-1's slot and first phase at ACME-2's `done`.
At 11:40 ACME-3's filing, ACME-1's planner's act 10 minutes into its planning segment, comes at its recorded time; ACME-1, a started parent with an open child, moves into `awaiting_children`, its planner stopped with 10 minutes of its segment left, and its slot goes to ACME-3, whose first phase starts in the same pass and runs its recorded course to its `done` at 13:20.
At 13:20 no open child is left: ACME-1 comes back to planning, takes a slot, and its planner works the rest of its segment to 13:30.
Its later segments, 10 minutes each, follow from that start: implementing to 13:40, testing to 13:50, reviewing to 14:00, accepting (its slot back) to its architect's sign-off at 14:10, which holds at once, since nothing beneath it is in the workflow; retro to 14:20 and merging to its `done` at 14:30.
Each issue's sequence of `workflow: phase changed` moves is the record's (the moves into and out of `awaiting_children` are lines of their own).

## The scorecard under `main`, worked by hand

The span is 7 hours.

**Slots.**
ACME-1 09:00 to 09:20 (20 minutes), 11:30 to 11:40 (10) and 13:20 to 14:00 (40); ACME-2 09:20 to 10:20 (60) and 10:30 to 11:00 (30); ACME-3 11:40 to 12:50 (70).
6 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 09:20, ACME-3 11:40.
Each was taken in the pass where its issue began to wait for it, ACME-1's return at 13:20 among them: all 0, median 0.

**Slot-hours.**
Every slot is held in a turn throughout: 20 + 10 + 40 + 60 + 30 + 70 = 230 minutes, `slotHoursInTurn` 3.8333… hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 14:30 (330 minutes), ACME-2 09:20 to 11:30 (130), ACME-3 11:40 to 13:20 (100): 560 minutes, 9.3333… hours, an average of 1.3333…, at most 2.

**Done and work left.**
ACME-2 11:30, ACME-3 13:20, ACME-1 14:30 (`done` 3); `workLeft` 0.

**Started work without a slot.**
ACME-1, given back for its children wait, from 09:20 to 11:30 (130 minutes), and in `awaiting_children` with no slot from 11:40 to 13:20 (100): reading (b) 230 minutes, 3.8333… hours, an average of 0.547619…, at most 1.
Neither is a wait only for a slot: reading (a) 0.

**Past the review.**
ACME-2 10:20 to 10:30 (10 minutes) and 11:00 to 11:30 (30), ACME-3 12:50 to 13:20 (30), ACME-1 14:00 to 14:30 (30), none with a slot and never two at once:
`pastReviewNotHolding` 100 minutes, 1.6666… hours, an average of 0.238095…, at most 1; `pastReviewHolding` 0.

**Priority taken.**
No issue waits for a slot: 0.

**Loops.**
As under `legion-v22.14.1`: `test_failed` 10 minutes, `send_back` 30 and `review_changes` 20, one round each.

**Waits on another issue.**
ACME-1's children wait at admitted, 09:20 to 11:30, and its wait in `awaiting_children`, 11:40 to ACME-3's `done` at 13:20, each ended by its last open child's `done`: `child_done`, count 2, 130 + 100 = 230 minutes, 3.8333… hours.

**Assumptions.**
As under `legion-v22.14.1`: `assumptions` is [1, 2, 3, 4, 7, 9, 10, 11, 13, 17] (11 is ACME-1's slot, freed at 09:20 and at 11:40, to the child in the same pass); `scenario` is `none`.

## The replay under `cover`

Spec v4's rule: a started issue keeps its slot in every phase, and an issue a counted waiter waits on starts at once on that waiter's slot, which it holds for the waiter while the cover stands (`record.TakeCover`).
To 11:30 it is the record's course but for the slots past the review: ACME-1's children wait at admitted gives its slot back (its first phase has not started), and ACME-2 keeps its slot through accepting, retro and merging, and through its send-back, to its `done` at 11:30.
At 11:40 ACME-3's filing comes at its recorded time: ACME-1 is a started parent with an open child, a waiter a cover reads (`record.SlotRule.StartedParents`, `blockers.CoverWaits`), so it keeps its phase, and ACME-3 starts on ACME-1's slot, which it holds for ACME-1 to its `done` at 13:20.
ACME-1's planner works its segment to 11:50 and moves it to implementing; its implementer's start waits for a slot until the cover ends at 13:20, where ACME-1 takes its slot again.
Then 10 minutes each: implementing to 13:30, testing to 13:40, reviewing to 13:50, accepting to its sign-off at 14:00, which holds at once, retro to 14:10 and merging to its `done` at 14:20, holding its slot to its leave.

## The scorecard under `cover`, worked by hand

The span is 7 hours.

**Slots.**
ACME-1 09:00 to 09:20 (20 minutes), 11:30 to 11:40 (10) and 13:20 to 14:20 (60); ACME-2 09:20 to 11:30 (130); ACME-3 11:40 to 13:20 (100).
5 slots on 2001-03-05: ACME-2 keeps its slot through its send-back; `firstSlot` ACME-1 09:00, ACME-2 09:20, ACME-3 11:40; all taken in the pass that owed them, median 0.

**Slot-hours.**
Every slot is held in a turn throughout, the architects' turns in accepting and the retro and merge turns among them: 20 + 10 + 60 + 130 + 100 = 320 minutes, `slotHoursInTurn` 5.3333… hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 14:20 (320 minutes), ACME-2 130, ACME-3 100: 550 minutes, 9.1666… hours, an average of 1.309523…, at most 2.

**Done and work left.**
ACME-2 11:30, ACME-3 13:20, ACME-1 14:20 (`done` 3); `workLeft` 0.

**Started work without a slot.**
ACME-1 given back for its children wait from 09:20 to 11:30 (130 minutes), and a covered waiter from 11:40 to 13:20 (100): reading (b) 230 minutes, 3.8333… hours, an average of 0.547619…, at most 1; reading (a) 0.

**Past the review.**
ACME-2 10:20 to 10:30 and 11:00 to 11:30 (40 minutes), ACME-3 12:50 to 13:20 (30), ACME-1 13:50 to 14:20 (30), each holding its slot and never two at once:
`pastReviewHolding` 100 minutes, 1.6666… hours, an average of 0.238095…, at most 1; `pastReviewNotHolding` 0.

**Priority taken.**
No issue waits for a slot: 0.

**Loops.**
As under `legion-v22.14.1`.

**Waits on another issue.**
ACME-1's children wait at admitted, 09:20 to 11:30, and its wait on ACME-3, 11:40 to ACME-3's `done` at 13:20, each ended by its last open child's `done`: `child_done`, count 2, 130 + 100 = 230 minutes, 3.8333… hours.
Under `cover` a started parent with an open child keeps its phase, and its wait on its open children counts in each phase from planning through accepting (`replay.md`, "The measures", `waits`), so ACME-1's wait on ACME-3 counts in planning and implementing, as it counts in `awaiting_children` under `main`; it is the wait the cover takes, whose 100 minutes reading (b) counts too.

**Assumptions.**
Those of `legion-v22.14.1`, and 14 (the cover of ACME-3 for ACME-1): `assumptions` is [1, 2, 3, 4, 7, 9, 10, 11, 13, 14, 17]; `scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all five signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
