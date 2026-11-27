# The window `send-back-at-full-cap`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
seven hours, from 2001-03-05T09:00Z to 16:00Z, an admission cap of 1, two issues, recorded under the rule `main`.
It holds one fact of check k1: a send-back from accepting at a full cap.
Under `main` ACME-1 holds no slot in accepting, ACME-2 has taken it, and the architect's send-back waits for a slot from 11:10 to 12:40;
under `keep-past-review` and `cover`, which keep a started issue's slot past its review, the send-back starts at once and ACME-2 waits instead.
`expect.json` holds the scorecards of a replay under each of the three rules on the main branch, worked by hand below;
nothing is drawn, so every seed gives them.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root handed to Legion that its architect sends back from accepting once | `a0` |
| ACME-2 | a root handed to Legion while the cap is full | `a1` |

## What happens (the record, under `main`)

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots; ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 1, architect turn 09:00 to 09:20 |
| 09:10 | ACME-2 filed, `todo`, `legion` | the cap is full: ACME-2 waits |
| 09:20 | ACME-1's design registered | planning; turn 09:20 to 09:40 |
| 09:40 | `planner_completed` | implementing; turn 09:40 to 10:10 |
| 10:10 | `implementation_ready` | testing; turn 10:10 to 10:30 |
| 10:30 | `tester_passed` | reviewing; turn 10:30 to 10:50 |
| 10:50 | ACME-1 `review_approved` | accepting; its slot goes back, and ACME-2 is promoted in the same pass: slot 1 of 1, architect turn 10:50 to 11:10; ACME-1's architect turn 10:50 to 11:10 |
| 11:10 | ACME-1's architect moves it back to implementing (`backward`) | its implementer's start is owed with `needsSlot`; the cap is full, so ACME-1 waits in the line's returned part from 11:10 |
| 11:10 | ACME-2's design registered | planning; turn 11:10 to 11:30 |
| 11:30, 12:00, 12:20 | ACME-2 `planner_completed`, `implementation_ready`, `tester_passed` | implementing 11:30 to 12:00, testing 12:00 to 12:20, reviewing 12:20 to 12:40 |
| 12:40 | ACME-2 `review_approved` | accepting; its slot goes back, and ACME-1 takes it in the same pass (`admission gives a slot back to an issue whose work came back`, implementing): implementer turn 12:40 to 13:10; ACME-2's architect turn 12:40 to 12:50 |
| 12:50 | ACME-2 `sign_off` | retro; turn 12:50 to 13:00 |
| 13:00 | ACME-2 `retro_completed` | merging; turn 13:00 to 13:20 |
| 13:10 | ACME-1 `implementation_ready` | testing; turn 13:10 to 13:30 |
| 13:20 | ACME-2 `pull_request_merged` | done |
| 13:30 | ACME-1 `tester_passed` | reviewing; turn 13:30 to 13:50 |
| 13:50 | ACME-1 `review_approved` | accepting; its slot goes back; architect turn 13:50 to 14:00 |
| 14:00 | ACME-1 `sign_off` | retro; turn 14:00 to 14:10 |
| 14:10 | ACME-1 `retro_completed` | merging; turn 14:10 to 14:30 |
| 14:30 | ACME-1 `pull_request_merged` | done |

ACME-1's segments: admitted 20, planning 20, implementing 30, testing 20, reviewing 20, accepting
20 (10:50 to 11:10, ending in the send-back); implementing again from its worker start at 12:40,
when it took the slot, to 13:10, 30; then testing 20, reviewing 20, accepting 10, retro 10, merging
20. ACME-2's: 20, 20, 30, 20, 20, 10, 10, 20 minutes.

## The replay under `main`

The recorded rule: every pass falls at its recorded time, and every segment starts at its recorded worker start with n = m.
The replay is the record.

## The replay under `keep-past-review`, and under `cover`

Both keep ACME-1's slot past its review, so at 10:50 nothing frees it and ACME-2 keeps waiting.
ACME-1's segments up to the send-back keep their recorded starts (09:00 to 11:10).
The send-back's implementer start finds ACME-1 holding its slot and starts at once:
implementing 11:10 to 11:40 (its recorded 30 minutes, from this worker start), testing 11:40 to 12:00, reviewing 12:00 to 12:20, accepting 12:20 to 12:30, retro 12:30 to 12:40, merging 12:40 to 13:00, merged at 13:00, when leaving the workflow gives the slot back and, in the same pass, ACME-2 is promoted.
ACME-2's segments run from 13:00: admitted 13:00 to 13:20, planning to 13:40, implementing to 14:10, testing to 14:30, reviewing to 14:50, accepting to 15:00, retro to 15:10, merging to 15:30, merged at 15:30, its slot kept through to its `done`.
Nobody waits on another issue, so `cover` takes no cover and gives the same values as `keep-past-review`.

## The scorecard under `main`, worked by hand

The span is 7 hours.

**Slots.**
ACME-1 09:00 to 10:50 (110 minutes), ACME-2 10:50 to 12:40 (110), ACME-1 12:40 to 13:50 (70): 3 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 10:50.
The waits: ACME-1 0; ACME-2 from 09:10 to 10:50, 100 minutes; ACME-1 from the send-back at 11:10 to 12:40, 90 minutes.
The median of 0, 100 and 90 is 90.

**Slot-hours.**
All three slots are held in a turn: ACME-1's five turns 09:00 to 10:50 (110), ACME-2's five 10:50 to 12:40 (110), ACME-1's implementer, tester and reviewer 12:40 to 13:50 (70).
290 minutes: `slotHoursInTurn` 4.8333… hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 14:30 (330 minutes), ACME-2 10:50 to 13:20 (150): 480 minutes, 8 hours, an average of 8/7 = 1.142857…, at most 2.

**Done and work left.**
ACME-2 at 13:20, ACME-1 at 14:30 (`done` 2); `workLeft` 0.

**Started work without a slot.**
ACME-1 from 11:10 to 12:40, owing its implementer's start and waiting on nothing but a slot:
readings (a) and (b) each 1.5 hours, an average of 0.214285…, at most
1.

**Past the review.**
ACME-1 10:50 to 11:10 (20 minutes) and 13:50 to 14:30 (40), ACME-2 12:40 to 13:20 (40), none with a slot:
`pastReviewNotHolding` 100 minutes, 1.6666… hours, an average of 0.238095…, at most 1; `pastReviewHolding` 0.

**Priority taken.**
09:10 to 10:50: ACME-2 (`a1`) waits while ACME-1 (`a0`) holds, a share of 0.
11:10 to 12:40: ACME-1 (`a0`) waits while ACME-2 (`a1`) holds, a share of 1.
The average is 1.5/7 = 0.214285….
No issue of the window has a Dispatch priority, so every issue stands in the band after P3 and the band moves no rank: `priorityTakenBanded`, the share in today's line's order, is the same 0.214285….

**Loops.**
One round of `send_back`, from the move back at 11:10 to ACME-1's return to accepting at 13:50: 160 minutes, 2.6666… hours.

**Waits.**
None.

**Assumptions.**
1; 2; 3 (the send-back keeps its count and cause);
9; 11 (ACME-1's slot, freed at 10:50, to ACME-2; ACME-2's, freed at 12:40, to ACME-1); 13 (the send-back moves ACME-1 back).
`assumptions` is [1, 2, 3, 9, 11, 13]; `scenario` is `none`.

## The scorecard under `keep-past-review`, and under `cover`, worked by hand

**Slots.**
ACME-1 09:00 to 13:00 (240 minutes), ACME-2 13:00 to 15:30 (150): 2 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 13:00.
The waits: 0, and ACME-2 from 09:10 to 13:00, 230 minutes; the median of 0 and 230 is 115.

**Slot-hours.**
Both slots are held in a turn throughout (ACME-1's turns run back to back from 09:00 to 13:00, ACME-2's from 13:00 to 15:30):
390 minutes, `slotHoursInTurn` 6.5 hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 13:00 (240), ACME-2 13:00 to 15:30 (150):
390 minutes, 6.5 hours, an average of 0.928571…, at most 1 (ACME-1 leaves at the instant ACME-2 starts).

**Done and work left.**
ACME-1 at 13:00, ACME-2 at 15:30 (`done` 2); `workLeft` 0.

**Started work without a slot.**
None: 0 in both readings.

**Past the review.**
ACME-1 10:50 to 11:10 (20 minutes) and 12:20 to 13:00 (40), ACME-2 14:50 to 15:30 (40), each holding its slot:
`pastReviewHolding` 100 minutes, 1.6666… hours, an average of 0.238095…, at most 1; `pastReviewNotHolding` 0.

**Priority taken.**
ACME-2 (`a1`) waits from 09:10 to 13:00 while ACME-1 (`a0`) holds: 0.

**Loops.**
One round of `send_back`, from 11:10 to ACME-1's return to accepting at 12:20: 70 minutes, 1.1666… hours.

**Waits.**
None.

**Assumptions.**
1; 2; 3; 9; 11 (ACME-1's slot, freed at its `done`, to ACME-2 in that pass); 13.
No cover is taken, so `cover` names no 14.
`assumptions` is [1, 2, 3, 9, 11, 13] under both; `scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under each rule that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
Each run limits all five signals (`limits`), and run.json echoes them; no rule here admits by signals, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
