# The window `one-minute-wait`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: six
hours, from 2001-03-05T09:00Z to 15:00Z, an admission cap of 1, three issues, recorded under the
rule `main`. It holds one fact of check k1: a wait on another issue that lasts one minute. A person
links ACME-1, a started root, to ACME-2 at 10:00 and takes the link off at 10:01. Under `main`, the
rule that made the record, the pass at 10:00 gives ACME-1's slot back and ACME-3 takes it, so ACME-1
waits behind ACME-3's work until 11:40 for a wait of one minute. Under `wait-grace`, whose grace is
five minutes, ACME-1 keeps its slot through the wait and ACME-3 waits for it instead. `expect.json`
holds the scorecard of a replay under each rule on the main branch, worked by hand below; nothing
is drawn, so every seed gives them.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root handed to Legion that runs every phase to `done` | `a0` |
| ACME-2 | an issue Legion does not work, in `backlog` all through | `a2` |
| ACME-3 | a root handed to Legion while the cap is full | `a1` |

## What happens

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots; ACME-2 filed, `backlog`, no label | |
| 09:05 | ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 1, architect turn 09:05 to 09:20 |
| 09:10 | ACME-3 filed, `todo`, `legion` | the cap is full: ACME-3 waits in the line's starting part |
| 09:20 | ACME-1's design registered (gate open) | planning by `gate_opened`; planner turn 09:20 to 10:20 |
| 10:00 | a person links ACME-1 `blocked_by: [ACME-2]` (`events/ACME-1.jsonl`, seq 3) | ACME-1 in planning waits on ACME-2, which is not `done`: its slot goes back (0 of 1); in the same pass ACME-3 is promoted: slot 1 of 1, architect turn 10:00 to 10:20. ACME-1's planner turn goes on |
| 10:01 | the person takes the link off ACME-1 (`blocked_by: []`, seq 4) | ACME-1's wait ends after one minute; the cap is full, so it waits only for a slot, first in the line's returned part |
| 10:20 | ACME-1 `planner_completed` | implementing; its implementer's start is owed and waits for a slot |
| 10:20 | ACME-3's design registered | planning; planner turn 10:20 to 10:40 |
| 10:40 | ACME-3 `planner_completed` | implementing; turn 10:40 to 11:00 |
| 11:00 | ACME-3 `implementation_ready` | testing; turn 11:00 to 11:20 |
| 11:20 | ACME-3 `tester_passed` | reviewing; turn 11:20 to 11:40 |
| 11:40 | ACME-3 `review_approved` | accepting; its slot goes back, and in the same pass ACME-1 takes it (`admission gives a slot back to an issue whose wait ended`, implementing; slot 1 of 1); ACME-1's implementer turn 11:40 to 12:20; ACME-3's architect turn 11:40 to 11:50 |
| 11:50 | ACME-3 `sign_off` | retro; turn 11:50 to 12:00 |
| 12:00 | ACME-3 `retro_completed` | merging; turn 12:00 to 12:10 |
| 12:10 | ACME-3 `pull_request_merged` | ACME-3 done |
| 12:20 | ACME-1 `implementation_ready` | testing; turn 12:20 to 12:40 |
| 12:40 | ACME-1 `tester_passed` | reviewing; turn 12:40 to 13:00 |
| 13:00 | ACME-1 `review_approved` | accepting; its slot goes back; architect turn 13:00 to 13:10 |
| 13:10 | ACME-1 `sign_off` | retro; turn 13:10 to 13:20 |
| 13:20 | ACME-1 `retro_completed` | merging; turn 13:20 to 13:30 |
| 13:30 | ACME-1 `pull_request_merged` | done |

ACME-1's segments: admitted 09:05 to 09:20 (15 minutes); planning 09:20 to 10:20 (60; its worker
started with the phase change, and the slot it gave back at 10:00 does not stop it); implementing
from its worker start at 11:40, when it took the slot, to 12:20 (40); then 20, 20, 10, 10 and 10
minutes. ACME-3's: admitted 10:00 to 10:20 (20, ended by its registration), then 20, 20, 20, 20,
10, 10 and 10 minutes.

## The replay under `main`

The recorded rule. The link's addition is a person's event at its recorded time. Its removal ends
ACME-1's wait, so the main branch's `Timing = offset` places it at its recorded offset from the
wait's start: the wait starts at 10:00 in the replay as in the record (the later of the link's
addition and ACME-1's start at 09:05), and the removal came one minute into it, at 10:01. Every pass
falls at its recorded time and every segment starts at its recorded worker start with n = m, so the
replay is the record.

## The scorecard under `main`, worked by hand

The span is 6 hours.

**Slots.** ACME-1 09:05 to 10:00 (55 minutes), ACME-3 10:00 to 11:40 (100), ACME-1 11:40 to 13:00
(80): 3 slots on 2001-03-05 (`slotsGivenByDay`); `firstSlot` ACME-1 09:05, ACME-3 10:00. The waits
for them: ACME-1 at 09:05, 0; ACME-3 from its filing at 09:10 to 10:00, 50 minutes; ACME-1 from the
end of its wait at 10:01 to 11:40, 99 minutes. The median of 0, 50 and 99 is 50
(`medianSlotWaitMinutes`).

**Slot-hours.** ACME-1 09:05 to 10:00: architect 09:05 to 09:20, planner 09:20 to 10:00, 55 minutes
in a turn (its planner's turn goes on to 10:20 with no slot). ACME-3 10:00 to 11:40: architect,
planner, implementer, tester, reviewer back to back, 100 minutes. ACME-1 11:40 to 13:00:
implementer, tester, reviewer, 80 minutes. `slotHoursInTurn` is 235 minutes, 3.91666… hours;
`slotHoursNoTurn` 0.

**In flight.** ACME-1 09:05 to 13:30 (265 minutes), ACME-3 10:00 to 12:10 (130): 395 minutes,
6.58333… hours, an average of 6.58333…/6 = 1.097222…, at most 2 (10:00 to 12:10).

**Done and work left.** ACME-3 done at 12:10 and ACME-1 at 13:30 (`done` 2, `doneAt`); `workLeft` 0.

**Started work without a slot.** ACME-1 is a returned member of the line with no slot from 10:00 to
11:40: reading (b) 100 minutes, 1.66666… hours, an average of 1.66666…/6 = 0.277777…, at most 1.
`admission.blocked` names it from 10:00 to 10:01, while it waits on ACME-2; from 10:01 to 11:40 it
waits only for a slot: reading (a) 99 minutes, 1.65 hours, an average of 1.65/6 = 0.275, at most 1.
So a wait of one minute costs ACME-1 1 hour 40 minutes without its slot.

**Past the review.** ACME-3 in accepting, retro and merging from 11:40 to 12:10, ACME-1 from 13:00
to 13:30, neither with a slot: `pastReviewNotHolding` 1 hour, an average of 1/6 = 0.16666…, at
most 1 (the two never overlap); `pastReviewHolding` 0.

**Priority taken.** Two stretches have an issue that waits for a slot. 09:10 to 10:00: ACME-3
(`a1`) waits, ACME-1 (`a0`) holds; the holder ranks above the waiter, a share of 0. 10:01 to 11:40:
ACME-1 (`a0`) waits, ACME-3 (`a1`) holds; the one pair has the holder below the waiter, a share of
1. From 10:00 to 10:01 ACME-1 waits on ACME-2, not for a slot, so no pair stands. The average is
1 × 99/60/6 = 0.275 (`priorityTaken`).
No issue of the window has a Dispatch priority, so every issue stands in the band after P3 and
the band moves no rank: `priorityTakenBanded`, the share in today's line's order, is the same
0.275 under `main` and 0 under `wait-grace`.

**Loops.** None.

**Waits on another issue.** ACME-1's wait on ACME-2, 10:00 to 10:01, ended by the link's removal:
`waits` holds `link_removed` with a count of 1 and 1/60 hour.

**Assumptions.** 1; 2 (ACME-3 and ACME-1 past their review); 4 (ACME-1's link wait); 9 (slots given
out); 10 (a link); 11 (ACME-1's slot, freed at 10:00, goes to ACME-3, and ACME-3's, freed at 11:40,
to ACME-1, each in the pass that freed it). Nothing is left at 15:00. `assumptions` is
[1, 2, 4, 9, 10, 11]; `scenario` is `none`.

## The replay under `wait-grace`

The rule with a grace of five minutes (`record.SlotRule.WaitGrace`). The link's addition at 10:00 is
a person's event, and its pass finds ACME-1 waiting on ACME-2 while it holds its slot: the wait is
younger than the grace, so the pass keeps the slot and marks the wait's start, 10:00, on it. The
removal falls at 10:01, one minute into the wait the replay began at 10:00, as under `main`; its
pass finds ACME-1 waiting on nothing and clears the mark. No pass gives the slot back, so ACME-1
stands in no place of the line, and ACME-3 waits in the starting part for the one slot.

| time (UTC) | what the replay does |
| --- | --- |
| 09:05 | ACME-1 promoted: slot 1 of 1, architect 09:05 to 09:20 |
| 09:20 | ACME-1 planning, planner 09:20 to 10:20 |
| 10:00 | the link's pass keeps ACME-1's slot inside the grace; ACME-3 still waits |
| 10:01 | the link taken off; the mark of the wait cleared |
| 10:20 | ACME-1 `planner_completed`: implementing, and its implementer starts at once on the slot it held: 10:20 to 11:00 (its 40 minutes) |
| 11:00 | testing, 11:00 to 11:20 |
| 11:20 | reviewing, 11:20 to 11:40 |
| 11:40 | ACME-1 `review_approved`: accepting, its slot goes back, and in the same pass ACME-3 is promoted: slot 1 of 1, architect 11:40 to 12:00 |
| 11:50, 12:00, 12:10 | ACME-1 retro, merging, done |
| 12:00 | ACME-3's design registered (20 minutes into its segment): planning, 12:00 to 12:20 |
| 12:20, 12:40, 13:00 | ACME-3 implementing, testing, reviewing |
| 13:20 | ACME-3 `review_approved`: accepting, its slot goes back |
| 13:30, 13:40, 13:50 | ACME-3 retro, merging, done |

Every segment keeps its recorded length from the replay's own worker start (f = 1).

## The scorecard under `wait-grace`, worked by hand

The span is 6 hours.

**Slots.** ACME-1 09:05 to 11:40 (155 minutes), ACME-3 11:40 to 13:20 (100): 2 slots on 2001-03-05
(`slotsGivenByDay`); `firstSlot` ACME-1 09:05, ACME-3 11:40. The waits for them: ACME-1 0; ACME-3
from its filing at 09:10 to 11:40, 150 minutes. The median of two values is the mean of the two,
75 (`medianSlotWaitMinutes`).

**Slot-hours.** ACME-1 09:05 to 11:40: architect 15, planner 60, implementer 40, tester 20 and
reviewer 20 minutes, 155 in a turn. ACME-3 11:40 to 13:20: architect, planner, implementer, tester
and reviewer back to back, 100. `slotHoursInTurn` 255 minutes, 4.25 hours; `slotHoursNoTurn` 0.

**In flight.** ACME-1 09:05 to 12:10 (185 minutes), ACME-3 11:40 to 13:50 (130): 315 minutes, 5.25
hours, an average of 5.25/6 = 0.875, at most 2 (11:40 to 12:10).

**Done and work left.** ACME-1 done at 12:10 and ACME-3 at 13:50; `workLeft` 0.

**Started work without a slot.** None: ACME-1 never stands in the line, and ACME-3 has not started
until it takes its slot. Both readings are 0.

**Past the review.** ACME-1 from 11:40 to 12:10, ACME-3 from 13:20 to 13:50, neither with a slot:
`pastReviewNotHolding` 1 hour, an average of 0.16666…, at most 1; `pastReviewHolding` 0.

**Priority taken.** 09:10 to 11:40: ACME-3 (`a1`) waits, ACME-1 (`a0`) holds, a share of 0. No other
stretch has a waiting issue: `priorityTaken` 0.

**Loops.** None.

**Waits on another issue.** The same wait as the record's: ACME-1 on ACME-2 from 10:00 to 10:01,
ended by the link's removal, with its slot kept: `link_removed`, a count of 1 and 1/60 hour.

**Assumptions.** 1; 2 (ACME-1 and ACME-3 past their review); 4 (ACME-1's link wait); 9; 10 (a link);
11 (ACME-1's slot, freed at 11:40, goes to ACME-3 in the pass that freed it). `assumptions` is
[1, 2, 4, 9, 10, 11]; `scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under each rule that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
Each run limits all five signals (`limits`), and run.json echoes them; neither rule here admits by signals, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
