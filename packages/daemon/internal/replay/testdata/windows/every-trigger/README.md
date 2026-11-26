# The window `every-trigger`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
seven hours, from 2001-03-05T09:00Z to 16:00Z, an admission cap of 2, a root and two children, recorded under the rule `main`.
It takes a root and two children through every trigger the workflow has: a failed test, a review that asks for changes, an architect's send-back from accepting, a sign-off held behind a child, the merge and the linger's expiry.
A replay under the recorded rule must give each issue the recorded sequence of (from, to, trigger), which `expect.json` holds under `moves` beside the scorecard, with no move refused.
`expect.json`'s scorecard is of a replay under `main` on the main branch, worked by hand below; nothing is drawn, so every seed gives it.

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

## The replay under `main`

The recorded rule.
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

## The scorecard under `main`, worked by hand

The span is 7 hours.

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
ACME-3 is filed while ACME-1 stands in planning, where no children wait holds it (`record.SlotRule.ChildrenFirstCandidates` reads an issue at admitted).

**Assumptions.**
1; 2; 3 (three loops keep their count and cause);
4 (ACME-1's children wait); 7 (the gate holds ACME-2's first start from 09:10 to 09:20);
9; 10 (children); 11 (ACME-1's slot, freed at 09:20, to ACME-2); 13 (the loops move issues back); 17 (both children filed after ACME-1 started).
`assumptions` is [1, 2, 3, 4, 7, 9, 10, 11, 13, 17]; `scenario` is `none`.

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
