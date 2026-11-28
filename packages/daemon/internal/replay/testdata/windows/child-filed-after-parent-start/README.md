# The window `child-filed-after-parent-start`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
six hours, from 2001-03-05T09:00Z to 15:00Z, an admission cap of 1, three issues, recorded under the rule `keep-past-review` (the daemon kept the slot past the review).
It holds one fact of check k1: a child filed after its parent starts moves with that start (spec R2:
a child's filing enters at its recorded offset from the start it depends on).
ACME-2's architect files ACME-3 20 minutes into its first segment.
Under `keep-past-review` ACME-2 starts at 13:00 and ACME-3 is filed at 13:20;
under `main`, ACME-1's slot goes back at its review, ACME-2 starts at 11:30, and ACME-3 is filed at 11:50.
`expect.json` holds the scorecards of a replay under each rule on the main branch, worked by hand below; nothing is drawn, so every seed gives them.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root handed to Legion that runs every phase to `done` | `a0` |
| ACME-2 | a root handed to Legion while the cap is full | `a1` |
| ACME-3 | a child of ACME-2, filed `todo` by ACME-2's architect | `a2` (its root's `a1` places it) |

## What happens (the record, under `keep-past-review`)

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots; ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 1, architect turn 09:00 to 09:30 |
| 09:10 | ACME-2 filed, `todo`, `legion` | the cap is full: ACME-2 waits |
| 09:30 | ACME-1's design registered | planning; turn 09:30 to 10:00 |
| 10:00, 10:30, 11:00 | ACME-1 `planner_completed`, `implementation_ready`, `tester_passed` | implementing 10:00 to 10:30, testing 10:30 to 11:00, reviewing 11:00 to 11:30 |
| 11:30 | ACME-1 `review_approved` | accepting, keeping its slot (the rule keeps one past the review); architect turn 11:30 to 12:00 |
| 12:00 | ACME-1 `sign_off` | retro; turn 12:00 to 12:30 |
| 12:30 | ACME-1 `retro_completed` | merging; turn 12:30 to 13:00 |
| 13:00 | ACME-1 `pull_request_merged` | done; leaving the workflow it gives its slot back (0 of 1), and in the same pass ACME-2 is promoted: slot 1 of 1, architect turn 13:00 to 13:30 |
| 13:20 | ACME-2's architect files ACME-3 under ACME-2, `todo` (`events/ACME-3.jsonl`, seq 1) | records ACME-3 at admitted in ACME-2's tree; the gate has no row, so the line holds it |
| 13:30 | ACME-2's architect ends its turn and registers its design (gate open) | ACME-2 at admitted under an open gate with an open child: its children wait begins and its slot goes back; in the same pass ACME-3's first phase starts: slot 1 of 1, planning by `gate_opened`, planner turn 13:30 to 14:00 |
| 14:00 | ACME-3 `planner_completed` | implementing; turn 14:00 to 14:30 |
| 14:30 | ACME-3 `implementation_ready` | testing; the tester's turn 14:30 is open at 15:00 |

The work segments: ACME-1 30 minutes in each of admitted, planning, implementing, testing,
reviewing, accepting, retro and merging (09:00 to 13:00); ACME-2 admitted from 13:00, with ACME-3's
filing at offset 20 and its design's registration at offset 30, a segment left open, since the visit
has no move out in the window (`../README.md`, "Work segments"); ACME-3 planning 13:30 to 14:00 (30), implementing 14:00
to 14:30 (30), and testing from 14:30, unended at 15:00 (its length is unknown: at least 30
minutes).

## The replay under `keep-past-review`

The recorded rule: every pass falls at its recorded time and every segment keeps its recorded start
and length, so the replay is the record.

## The replay under `main`

`main` keeps no slot in accepting, retro or merging.
ACME-1's segments start at their recorded worker starts (it holds its slot from 09:00 through its review either way) and keep their lengths, so it reaches accepting at 11:30 and merges at 13:00, as recorded.
At 11:30 its slot goes back and, in the same pass, ACME-2, the line's one member, is promoted: slot 1 of 1, its architect's segment open from 11:30, as in the record (the visit has no move out in the window).
ACME-3's filing enters at its recorded offset in that segment, 20 minutes, at 11:50, held by the gate.
At 12:00 ACME-2 registers its design; its children wait begins and its slot goes to ACME-3, whose segments run planning 12:00 to 12:30 and implementing 12:30 to 13:00;
its testing segment starts at 13:00 and, its recorded length unknown, does not end before 15:00, its tester's turn open from 13:00 on.
ACME-1's segments past its review keep their lengths with no slot (accepting 11:30 to 12:00, retro 12:00 to 12:30, merging 12:30 to 13:00).

## The scorecard under `keep-past-review`, worked by hand

The span is 6 hours.

**Slots.**
ACME-1 09:00 to 13:00 (240 minutes), ACME-2 13:00 to 13:30 (30), ACME-3 13:30 to 15:00 (90):
3 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 13:00, ACME-3 13:30.
The waits: ACME-1 0; ACME-2 from 09:10 to 13:00, 230 minutes; ACME-3 0 (the gate held it from 13:20, and the pass that opened the gate gave it its slot).
The median of 0, 0 and 230 is 0.

**Slot-hours.**
Every slot is held with a worker in a turn:
ACME-1's eight turns back to back from 09:00 to 13:00 (240 minutes), ACME-2's architect 13:00 to 13:30 (30), ACME-3's planner, implementer and tester from 13:30 to 15:00 (90).
`slotHoursInTurn` 360 minutes, 6 hours; `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 13:00 (240), ACME-2 13:00 to 15:00 (120), ACME-3 13:30 to 15:00 (90): 450 minutes, 7.5 hours, an average of 1.25, at most 2 (13:30 to 15:00).

**Done and work left.**
ACME-1 done at 13:00 (`done` 1); ACME-2 and ACME-3 in flight at 15:00 (`workLeft` 2).

**Started work without a slot.**
ACME-2, given back for its children wait, from 13:30 to 15:00: reading (b) 1.5 hours, an average of 0.25, at most 1.
`admission.blocked` names it throughout: reading (a) 0.

**Past the review.**
ACME-1 in accepting, retro and merging from 11:30 to 13:00, holding its slot:
`pastReviewHolding` 1.5 hours, an average of 0.25, at most 1; `pastReviewNotHolding` 0.

**Priority taken.**
ACME-2 (`a1`) waits for a slot from 09:10 to 13:00 while ACME-1 (`a0`) holds: a share of 0.
ACME-3 never waits for one.
`priorityTaken` 0.

**Loops.**
None.

**Waits on another issue.**
ACME-2's children wait from 13:30, still open at 15:00: `waits` holds `open` with a count of 1 and 1.5 hours.

**Assumptions.**
1; 2 (ACME-1 past its review); 4 (ACME-2's children wait);
7 (the gate holds ACME-3's first start from 13:20 to 13:30);
9; 10 (a child); 11 (ACME-1's slot, freed at 13:00, to ACME-2;
ACME-2's, freed at 13:30, to ACME-3); 15 (ACME-2 and ACME-3 in flight at 15:00); 17 (ACME-3 filed after ACME-2 started).
`assumptions` is [1, 2, 4, 7, 9, 10, 11, 15, 17]; `scenario` is `none`.

## The scorecard under `main`, worked by hand

**Slots.**
ACME-1 09:00 to 11:30 (150 minutes), ACME-2 11:30 to 12:00 (30), ACME-3 12:00 to 15:00 (180):
3 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 11:30, ACME-3 12:00.
The waits: 0; ACME-2 09:10 to 11:30, 140 minutes; ACME-3 0.
The median is 0.

**Slot-hours.**
ACME-1's five turns from 09:00 to 11:30 (150 minutes), ACME-2's architect 11:30 to 12:00 (30), ACME-3's planner 12:00 to 12:30, implementer 12:30 to 13:00 and tester 13:00 to 15:00 (180).
`slotHoursInTurn` 360 minutes, 6 hours; `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 13:00 (240), ACME-2 11:30 to 15:00 (210), ACME-3 12:00 to 15:00 (180):
630 minutes, 10.5 hours, an average of 1.75, and 3 at once from 12:00 to 13:00.

**Done and work left.**
ACME-1 done at 13:00 (`done` 1); `workLeft` 2.

**Started work without a slot.**
ACME-2 from 12:00 to 15:00: reading (b) 3 hours, an average of 0.5, at most 1; reading (a) 0.

**Past the review.**
ACME-1 from 11:30 to 13:00 with no slot: `pastReviewNotHolding` 1.5 hours, an average of 0.25, at most 1; `pastReviewHolding` 0.

**Priority taken.**
ACME-2 (`a1`) waits from 09:10 to 11:30 while ACME-1 (`a0`) holds: 0.

**Loops.**
None.
**Waits.**
ACME-2's children wait from 12:00, open at 15:00: `open`, count 1, 3 hours.

**Assumptions.**
The same facts, at their own times: [1, 2, 4, 7, 9, 10, 11, 15, 17] (7:
the gate holds ACME-3 from 11:50 to 12:00; 11: ACME-1's slot, freed at 11:30, to ACME-2, and ACME-2's, freed at 12:00, to ACME-3).
`scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under each rule that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
Each run limits all five signals (`limits`), and run.json echoes them; neither rule here admits by signals, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
