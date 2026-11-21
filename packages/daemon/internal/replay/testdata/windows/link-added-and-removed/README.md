# The window `link-added-and-removed`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: seven
hours, from 2001-03-05T09:00Z to 16:00Z, an admission cap of 1, three issues, recorded under the
rule `main`. It holds one fact of check k1: a `blocked_by` link added and taken off inside the
window, each at the time its event gives it (spec R2: each link has the lifetime its Dispatch events
give it). ACME-1 gives its slot back when a person links it to ACME-2 at 10:00, ACME-3 takes the
slot, and when the person takes the link off at 11:00 ACME-1 waits only for a slot until 12:30.
`expect.json` holds the scorecard of a replay under `main` on the main branch, worked by hand below;
nothing is drawn, so every seed gives it.

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
| 09:05 | ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 1, architect turn 09:05 to 09:30 |
| 09:10 | ACME-3 filed, `todo`, `legion` | the cap is full: ACME-3 waits in the line's starting part |
| 09:30 | ACME-1's design registered (gate open) | planning by `gate_opened`; planner turn 09:30 to 10:30 |
| 10:00 | a person links ACME-1 `blocked_by: [ACME-2]` (`events/ACME-1.jsonl`, seq 3) | ACME-1 in planning waits on ACME-2, which is not `done`: its slot goes back (0 of 1); in the same pass ACME-3, the first member waiting only for the cap, is promoted: slot 1 of 1, architect turn 10:00 to 10:30. ACME-1's planner turn goes on. |
| 10:30 | ACME-1 `planner_completed` | implementing; its implementer's start is owed and waits (ACME-1 holds no slot and waits on ACME-2) |
| 10:30 | ACME-3's design registered | ACME-3 planning; planner turn 10:30 to 11:00 |
| 11:00 | the person takes the link off ACME-1 (`blocked_by: []`, seq 4) | ACME-1's wait ends; it owes a start and the cap is full, so it waits only for a slot, first in the line's returned part |
| 11:00 | ACME-3 `planner_completed` | implementing; turn 11:00 to 11:30 |
| 11:30 | ACME-3 `implementation_ready` | testing; turn 11:30 to 12:00 |
| 12:00 | ACME-3 `tester_passed` | reviewing; turn 12:00 to 12:30 |
| 12:30 | ACME-3 `review_approved` | accepting; its slot goes back, and in the same pass ACME-1 takes it (`admission gives a slot back to an issue whose wait ended`, implementing; slot 1 of 1); ACME-1's implementer turn 12:30 to 13:30; ACME-3's architect turn 12:30 to 12:45 |
| 12:45 | ACME-3 `sign_off` | retro; turn 12:45 to 13:00 |
| 13:00 | ACME-3 `retro_completed` | merging; turn 13:00 to 13:30 |
| 13:30 | ACME-3 `pull_request_merged`; ACME-1 `implementation_ready` | ACME-3 done; ACME-1 testing, turn 13:30 to 14:00 |
| 14:00 | ACME-1 `tester_passed` | reviewing; turn 14:00 to 14:30 |
| 14:30 | ACME-1 `review_approved` | accepting; its slot goes back; architect turn 14:30 to 14:45 |
| 14:45 | ACME-1 `sign_off` | retro; turn 14:45 to 15:00 |
| 15:00 | ACME-1 `retro_completed` | merging; turn 15:00 to 15:30 |
| 15:30 | ACME-1 `pull_request_merged` | done |

ACME-1's segments: admitted 09:05 to 09:30 (25 min); planning 09:30 to 10:30 (60; its worker
started with the phase change at 09:30, and the slot it gave back at 10:00 does not stop it);
implementing from its worker start at 12:30, when it took the slot, to 13:30 (60); then 30, 30, 15,
15 and 30 minutes. ACME-3's: 30, 30, 30, 30, 30, then 15, 15 and 30 minutes.

## The replay under `main`

The recorded rule. The link's addition is a person's event at its recorded time. Its removal ends
ACME-1's wait, so the main branch's `Timing = offset` places it at its recorded offset from the
wait's start: the wait starts at 10:00 in the replay as in the record (the later of the link's
addition and ACME-1's start at 09:05), and the removal came 60 minutes into it, at 11:00. Every
pass falls at its recorded time and every segment starts at its recorded worker start with n = m,
so the replay is the record. A replay that merged the link over the window (assumption 10's fault
of the first replay) would keep ACME-1 waiting on ACME-2 to the end and never give it a slot again.

## The scorecard under `main`, worked by hand

The span is 7 hours.

**Slots.** ACME-1 09:05 to 10:00 (55 minutes), ACME-3 10:00 to 12:30 (150), ACME-1 12:30 to 14:30
(120): 3 slots on 2001-03-05 (`slotsGivenByDay`); `firstSlot` ACME-1 09:05, ACME-3 10:00. The
waits for them: ACME-1 at 09:05, 0; ACME-3 from its filing at 09:10 to 10:00, 50 minutes; ACME-1
from the link's removal at 11:00 to 12:30, 90 minutes. The median of 0, 50 and 90 is 50
(`medianSlotWaitMinutes`).

**Slot-hours.** ACME-1 09:05 to 10:00: architect 09:05 to 09:30, planner 09:30 to 10:00, 55 minutes
in a turn (its planner's turn goes on to 10:30 with no slot). ACME-3 10:00 to 12:30: architect,
planner, implementer, tester, reviewer back to back, 150 minutes. ACME-1 12:30 to 14:30:
implementer, tester, reviewer, 120 minutes. `slotHoursInTurn` is 325 minutes, 5.41666… hours;
`slotHoursNoTurn` 0.

**In flight.** ACME-1 09:05 to 15:30 (385 minutes), ACME-3 10:00 to 13:30 (210): 595 minutes,
9.91666… hours, an average of 9.91666…/7 = 1.41666…, at most 2 (10:00 to 13:30).

**Done and work left.** ACME-3 done at 13:30 and ACME-1 at 15:30 (`done` 2, `doneAt`); `workLeft` 0.

**Started work without a slot.** ACME-1 is a returned member of the line with no slot from 10:00 to
12:30: reading (b) 2.5 hours, an average of 2.5/7 = 0.357142…, at most 1. `admission.blocked`
names it from 10:00 to 11:00, while it waits on ACME-2; from 11:00 to 12:30 it waits only for a
slot: reading (a) 1.5 hours, an average of 1.5/7 = 0.214285…, at most 1.

**Past the review.** ACME-3 in accepting, retro and merging from 12:30 to 13:30, ACME-1 from 14:30
to 15:30, neither with a slot: `pastReviewNotHolding` 2 hours, an average of 2/7 = 0.285714…, at
most 1 (the two never overlap); `pastReviewHolding` 0.

**Priority taken.** Two stretches have an issue that waits for a slot. 09:10 to 10:00: ACME-3
(`a1`) waits, ACME-1 (`a0`) holds; the holder ranks above the waiter, a share of 0. 11:00 to 12:30:
ACME-1 (`a0`) waits, ACME-3 (`a1`) holds; the one pair has the holder below the waiter, a share of
1. The share is 0 at every other instant (no pair). The average is 1 × 1.5/7 = 0.214285…
(`priorityTaken`).

**Loops.** None.

**Waits on another issue.** ACME-1's wait on ACME-2, 10:00 to 11:00, ended by the link's removal:
`waits` holds `link_removed` with a count of 1 and 1 hour.

**Assumptions.** 1; 2 (ACME-3 and ACME-1 past their review); 4 (ACME-1's link wait); 9 (slots given
out); 10 (a link); 11 (ACME-1's slot, freed at 10:00, goes to ACME-3, and ACME-3's, freed at 12:30,
to ACME-1, each in the pass that freed it). Nothing is left at 16:00. `assumptions` is
[1, 2, 4, 9, 10, 11]; `scenario` is `none`.
