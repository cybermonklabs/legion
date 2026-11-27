# The window `forty-two-more-in-flight`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
three hours, from 2001-03-05T09:00Z to 12:00Z, an admission cap of 43, 85 issues, recorded under the rule `keep-past-review`.
It holds one fact of check k1: a segment that starts with 42 more issues in flight than the record had takes f times its recorded length, and one that starts at the recorded number keeps it (spec R3).
In the record, 42 issues sit in accepting holding their slots from 09:50, and 42 queued roots never start.
Replayed under `main`, those 42 slots go back at 09:50, the 42 queued roots start, and the probe issue ACME-43 starts its implementing segment at 10:00 with 85 issues in flight where the record had 43.
Both scorecards of `expect.json` are of a replay at f = 1.5 (their `slowdown`), on the main branch, worked by hand below;
nothing is drawn, so every seed gives them.

## The issues

| issues | what they are | ranks |
| --- | --- | --- |
| ACME-1 to ACME-42 | 42 roots filed at 09:00, each through its review by 09:50, then in accepting to the end | `a01` to `a42` |
| ACME-43 | the probe: a root filed at 09:00 with a long implementing phase | `a43` |
| ACME-44 to ACME-85 | 42 roots filed at 09:01, while the cap is full | `b01` to `b42` |

## What happens (the record, under `keep-past-review`)

| time (UTC) | ACME-1 to ACME-42, each | ACME-43 | ACME-44 to ACME-85 |
| --- | --- | --- | --- |
| 09:00 | filed and promoted, slots 1 to 42 of 43; architect turn to 09:10 | filed and promoted, slot 43 of 43; architect turn to 09:20 | |
| 09:01 | | | filed, each in turn: the cap is full, all 42 wait |
| 09:10 | design registered: planning, turn to 09:20 | | |
| 09:20 | implementing, turn to 09:30 | design registered: planning, turn to 10:00 | |
| 09:30 | testing, turn to 09:40 | | |
| 09:40 | reviewing, turn to 09:50 | | |
| 09:50 | `review_approved`: accepting, keeping its slot; no further turn | | |
| 10:00 | | implementing, turn to 11:00 | |
| 11:00 | | testing, turn to 11:30 | |
| 11:30 | | reviewing, turn to 11:50 | |
| 11:50 | | accepting, keeping its slot; architect turn open at 12:00 | |

No slot comes free in the record: the 42 queued roots never start.

The work segments, with the issues in flight in the record at each recorded start (m):

| issue | phase | recorded worker start | length | m |
| --- | --- | --- | --- | --- |
| each of ACME-1 to ACME-42 | admitted, planning, implementing, testing, reviewing | 09:00, 09:10, 09:20, 09:30, 09:40 | 10 min each | 43 |
| each of ACME-1 to ACME-42 | accepting | 09:50 | unended at 12:00 | 43 |
| ACME-43 | admitted | 09:00 | 20 min | 43 |
| ACME-43 | planning | 09:20 | 40 min | 43 |
| ACME-43 | implementing | 10:00 | 60 min | 43 |
| ACME-43 | testing | 11:00 | 30 min | 43 |
| ACME-43 | reviewing | 11:30 | 20 min | 43 |
| ACME-43 | accepting | 11:50 | unended at 12:00 | 43 |

## The replay under `keep-past-review` at f = 1.5

The recorded rule: the replay's issues in flight at every start are the record's (n = m = 43), so
every segment keeps its length, 1.5⁰ = 1, and the replay is the record.

## The replay under `main` at f = 1.5

Up to 09:50 every pass decides as the record's (the slots are all held before any review), so every
segment that starts before 09:50 starts with n = m = 43 and keeps its length: ACME-43's planning
segment, which starts at 09:20 with 43 in flight in the replay and in the record, keeps its 40
minutes.

At 09:50 each of ACME-1 to ACME-42 enters accepting and, under `main`, gives its slot back;
each pass gives the freed slot to the line's first member, so ACME-44 to ACME-85 all take a slot at 09:50, in rank order.
They are issues handed to Legion that the record never started, so the main branch's scenario `none` gives them no work:
each holds its slot from 09:50 to 12:00 with no worker in a turn.
ACME-1 to ACME-42 are not done, so they stay in flight; nothing ends a segment of theirs.

At 10:00 ACME-43 starts implementing with n = 42 + 1 + 42 = 85 issues in flight in the replay, where the record had m = 43 at its start:
its length is 60 × 1.5^((85 − 43)/42) = 60 × 1.5 = 90 minutes, so it ends at 11:30, not 11:00.
Its testing segment starts at 11:30 with n = 85 and m = 43 (the record's at 11:00): 30 × 1.5 = 45 minutes, ending at 12:15, after the window.
So under `main` ACME-43 is in testing at 12:00, its tester's turn open from 11:30, and it never reaches its review.
(Run with `--trace-segments`, the replay's `segments.jsonl` holds ACME-43's planning segment with n = m = 43 and a length of 40 minutes, and its implementing segment with n = 85, m = 43, recorded 60 and length 90 minutes.)

## The scorecard under `keep-past-review` at f = 1.5, worked by hand

The span is 3 hours.

**Slots.**
43 slots given at 09:00 (ACME-1 to ACME-43), each held to 12:00: `slotsGivenByDay` 43, `firstSlot` 09:00 for each of the 43.
Each was taken in the pass of its own filing, so all 43 waits are 0 and the median is 0.

**Slot-hours.**
Each of ACME-1 to ACME-42 is in a turn from 09:00 to 09:50 (five turns of 10 minutes) and in none from 09:50 to 12:00: 50 and 130 minutes.
ACME-43 is in a turn from 09:00 to 12:00 (its turns run back to back; the architect's in accepting is open at the end): 180 minutes.
`slotHoursInTurn` 42 × 50 + 180 = 2280 minutes, 38 hours; `slotHoursNoTurn` 42 × 130 = 5460 minutes, 91 hours.

**In flight.**
43 issues from 09:00 to 12:00: 43 × 3 = 129 hours, an average of 43, at most 43.

**Done and work left.**
None done (`done` 0); all 43 in flight at 12:00 (`workLeft` 43).

**Started work without a slot.**
None: 0 in both readings.

**Past the review.**
ACME-1 to ACME-42 in accepting from 09:50 to 12:00 (130 minutes each) and ACME-43 from 11:50 to 12:00 (10), each holding its slot:
`pastReviewHolding` 42 × 130 + 10 = 5470 minutes, 91.1666… hours, an average of 30.38888…, and 43 at once from 11:50; `pastReviewNotHolding`
0.

**Priority taken.**
ACME-44 to ACME-85 (`b01` to `b42`) wait from 09:01 to 12:00 under 43 holders ranked `a01` to `a43`, all above them: `priorityTaken` 0.

**Loops and waits.**
None.

**Assumptions.**
1 (f scales every segment, here by 1.5⁰);
2 (issues past their review); 8 (ACME-44 to ACME-85, handed to Legion, never started in the record); 9; 15 (43 issues in flight at 12:00).
No slot is freed.
`assumptions` is [1, 2, 8, 9, 15]; `scenario` is `none`.

## The scorecard under `main` at f = 1.5, worked by hand

**Slots.**
43 slots at 09:00 and 42 at 09:50: `slotsGivenByDay` 85; `firstSlot` 09:00 for ACME-1 to ACME-43 and 09:50 for ACME-44 to ACME-85.
The waits: 43 of 0, and 42 of 49 minutes (09:01 to 09:50).
Of the 85 sorted waits the 43rd, the median, is 0.

**Slot-hours.**
ACME-1 to ACME-42 hold theirs from 09:00 to 09:50, in a turn throughout: 42 × 50 = 2100 minutes.
ACME-43 holds its slot from 09:00 to 12:00 in a turn throughout (architect, planner, implementer 10:00 to 11:30, tester from 11:30): 180 minutes.
ACME-44 to ACME-85 hold theirs from 09:50 to 12:00 with no turn: 42 × 130 = 5460 minutes.
`slotHoursInTurn` 2280 minutes, 38 hours; `slotHoursNoTurn` 5460 minutes, 91 hours.

**In flight.**
ACME-1 to ACME-43 from 09:00 to 12:00 (43 × 180 = 7740 minutes), ACME-44 to ACME-85 from 09:50 to 12:00 (42 × 130 = 5460):
13200 minutes, 220 hours, an average of 73.333…, and 85 at once from 09:50.

**Done and work left.**
None done; all 85 in flight at 12:00 (`workLeft` 85).

**Started work without a slot.**
None: 0 in both readings.

**Past the review.**
ACME-1 to ACME-42 from 09:50 to 12:00 with no slot: `pastReviewNotHolding` 5460 minutes, 91 hours, an average of 30.333…, at most 42.
ACME-43 never reaches its review: `pastReviewHolding` 0.

**Priority taken.**
ACME-44 to ACME-85 wait from 09:01 to 09:50 under holders all ranked above them:
0.

**Loops and waits.**
None.

**Assumptions.**
1 (f scales ACME-43's segments); 2; 8; 9; 11 (each slot freed at 09:50 goes to a queued root in that pass); 15 (85 in flight at 12:00).
`assumptions` is [1, 2, 8, 9, 11, 15]; `scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under each rule that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
Each run limits all five signals (`limits`), and run.json echoes them; neither rule here admits by signals, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
