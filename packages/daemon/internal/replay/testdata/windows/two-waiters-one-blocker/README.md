# The window `two-waiters-one-blocker`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: seven
hours, from 2001-03-05T09:00Z to 16:00Z, an admission cap of 2, four issues, recorded under the rule
`main`. It holds one fact of check k1: two started issues wait on one blocker. ACME-1 and ACME-2
are linked to ACME-3, a root Legion has not started. Under `main` each gives its slot back, ACME-3
and ACME-4 take them, and when ACME-3 is done the two waiters come back one at a time. Under
`cover` the waiters keep their slots, ACME-3 starts at once on ACME-1's slot as its cover, and
ACME-4 waits. `expect.json` holds the scorecards of a replay under each rule on the main branch,
worked by hand below; nothing is drawn, so every seed gives them.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root, started first; a person links it to ACME-3 at 09:40 | `a0` |
| ACME-2 | a root, started second; a person links it to ACME-3 at 09:45 | `a1` |
| ACME-3 | a root both wait on, filed while the cap is full | `a2` |
| ACME-4 | a root filed while the cap is full, with a long implementing phase | `a3` |

## What happens (the record, under `main`)

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots; ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 2, architect turn 09:00 to 09:20 |
| 09:05 | ACME-2 filed | promotes it: slot 2 of 2, architect turn 09:05 to 09:25 |
| 09:10, 09:15 | ACME-3 filed, then ACME-4 | the cap is full: both wait, ACME-3 first by rank |
| 09:20, 09:25 | ACME-1's, then ACME-2's design registered | each to planning; planner turns 09:20 to 10:00 and 09:25 to 10:05 |
| 09:40 | a person links ACME-1 `blocked_by: [ACME-3]` (seq 3) | ACME-1 in planning waits on ACME-3, which is not `done`: its slot goes back (1 of 2), and ACME-3 is promoted in the same pass (2 of 2), architect turn 09:40 to 10:00. ACME-1's planner turn goes on. |
| 09:45 | a person links ACME-2 `blocked_by: [ACME-3]` (seq 3) | ACME-2's slot goes back, and ACME-4 is promoted (2 of 2), architect turn 09:45 to 10:05 |
| 10:00 | ACME-1 `planner_completed`; ACME-3's design registered | ACME-1 to implementing, its start owed and waiting (no slot, a wait); ACME-3 planning, turn 10:00 to 10:20 |
| 10:05 | ACME-2 `planner_completed`; ACME-4's design registered | ACME-2 to implementing, its start owed and waiting; ACME-4 planning, turn 10:05 to 10:25 |
| 10:20, 10:25 | ACME-3, ACME-4 `planner_completed` | implementing: ACME-3 10:20 to 10:50, ACME-4 10:25 to 12:30 |
| 10:50, 11:10 | ACME-3 `implementation_ready`, `tester_passed` | testing 10:50 to 11:10, reviewing 11:10 to 11:30 |
| 11:30 | ACME-3 `review_approved` | accepting, slot back (1 of 2); both waiters still wait on ACME-3, so nothing takes it; architect turn 11:30 to 11:40 |
| 11:40, 11:50 | ACME-3 `sign_off`, `retro_completed` | retro 11:40 to 11:50, merging 11:50 to 12:10 |
| 12:10 | ACME-3 `pull_request_merged`, and the daemon's `done` (seq 6) | both waits end; the line's returned part holds ACME-1 (given back at 09:40) and ACME-2 (09:45), each owing its implementer's start; one slot is free: ACME-1 takes it (`admission gives a slot back to an issue whose wait ended`, 2 of 2), implementer turn 12:10 to 12:40. ACME-2 waits only for a slot. |
| 12:30, 12:50 | ACME-4 `implementation_ready`, `tester_passed` | testing 12:30 to 12:50, reviewing 12:50 to 13:10 |
| 12:40, 13:00 | ACME-1 `implementation_ready`, `tester_passed` | testing 12:40 to 13:00, reviewing 13:00 to 13:20 |
| 13:10 | ACME-4 `review_approved` | accepting, slot back; ACME-2 takes it in the same pass, implementer turn 13:10 to 13:40 |
| 13:20 to 13:50 | ACME-1 to accepting (slot back), retro, merging; ACME-4 retro, merging | ACME-4 merged at 13:50 |
| 14:00 | ACME-1 `pull_request_merged`; ACME-2 `tester_passed` | ACME-1 done; ACME-2 reviewing 14:00 to 14:20 |
| 14:20 to 15:00 | ACME-2 to accepting (slot back), retro, merging | merged at 15:00 |

The segments: ACME-1 admitted 20, planning 40 (09:20 to 10:00, going on past its 09:40 release),
implementing 30 from its worker start at 12:10, testing 20, reviewing 20, accepting 10, retro 10,
merging 20. ACME-2 the same lengths: admitted 09:05 to 09:25, planning 09:25 to 10:05, implementing
from 13:10. ACME-3: 20, 20, 30, 20, 20, 10, 10, 20 from 09:40. ACME-4: admitted 20, planning 20,
implementing 125 (10:25 to 12:30), testing 20, reviewing 20, accepting 10, retro 10, merging 20 from
09:45.

## The replay under `main`

The recorded rule. The two links are a person's events at their recorded times; ACME-3's `done` is
the replay's own merge of a Legion issue (no timed event), and the replay's echo of its own `done`
write enters at that instant, 12:10, as the record shows it. Every segment starts at its recorded
worker start with n = m: the replay is the record.

## The replay under `cover`

`cover` keeps a started issue's slot until its work is done and starts an issue that a slot holder
waits on at once on that holder's slot (`../README.md`, Starts and slots).

- 09:40: ACME-1 waits on ACME-3, which has not started: ACME-3 starts on ACME-1's slot as its cover
  (a slot given, past the cap of 2, and ACME-1 holds none of its own while covered); its segments
  run from 09:40, as recorded: merged at 12:10.
- 09:45: ACME-2 waits on ACME-3 too, which is already started under ACME-1's cover: one cover for
  each holder, so ACME-2 keeps its own slot and counts. ACME-4 stays in the line: the slots in use
  are ACME-3's (on ACME-1's) and ACME-2's.
- ACME-1's and ACME-2's planning phases go on (to 10:00 and 10:05) and their implementing starts
  wait for their wait to end; ACME-2 holds its slot with no worker in a turn from 10:05.
- 12:10: ACME-3 is done; ACME-1 takes its slot back at once, and both waits end: both implementers
  start at 12:10, and each runs its recorded segments: implementing 12:10 to 12:40, testing to
  13:00, reviewing to 13:20, accepting to 13:30, retro to 13:40, merging to 14:00, keeping its slot
  to its `done` at 14:00.
- 14:00: two slots come free; ACME-4, the only member of the line, takes one: architect 14:00 to
  14:20, planning 14:20 to 14:40, implementing from 14:40, whose 125 minutes run past 16:00.

## The scorecard under `main`, worked by hand

The span is 7 hours.

**Slots.** ACME-1 09:00 to 09:40 (40 minutes) and 12:10 to 13:20 (70); ACME-2 09:05 to 09:45 (40) and
13:10 to 14:20 (70); ACME-3 09:40 to 11:30 (110); ACME-4 09:45 to 13:10 (205). 6 slots on
2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 09:05, ACME-3 09:40, ACME-4 09:45. The waits: ACME-1 0,
ACME-2 0, ACME-3 from 09:10 to 09:40 (30), ACME-4 from 09:15 to 09:45 (30), ACME-1 at 12:10 0 (its
wait ended in that pass), ACME-2 from 12:10 to 13:10 (60). Sorted: 0, 0, 0, 30, 30, 60; the median is
the mean of the third and fourth, 15.

**Slot-hours.** ACME-1 09:00 to 09:40: architect 20, planner 20 (40 in a turn); 12:10 to 13:20:
implementer, tester, reviewer (70). ACME-2 09:05 to 09:45: architect 20, planner 20 (40); 13:10 to
14:20: 70. ACME-3 09:40 to 11:30: five turns, 110. ACME-4 09:45 to 13:10: architect, planner,
implementer, tester, reviewer, 205. Every slot is in a turn throughout: 535 minutes,
`slotHoursInTurn` 8.91666… hours, `slotHoursNoTurn` 0.

**In flight.** ACME-1 09:00 to 14:00 (300 minutes), ACME-2 09:05 to 15:00 (355), ACME-3 09:40 to
12:10 (150), ACME-4 09:45 to 13:50 (245): 1050 minutes, 17.5 hours, an average of 2.5, and 4 at once
from 09:45 to 12:10.

**Done and work left.** ACME-3 12:10, ACME-4 13:50, ACME-1 14:00, ACME-2 15:00 (`done` 4); `workLeft`
0.

**Started work without a slot.** ACME-1 from 09:40 to 12:10 and ACME-2 from 09:45 to 13:10 are
returned members holding no slot: reading (b) 150 + 205 = 355 minutes, 5.91666… hours, an average of
0.845238…, and 2 at once from 09:45 to 12:10. `admission.blocked` names both until 12:10; ACME-2
then waits only for a slot to 13:10: reading (a) 1 hour, an average of 0.142857…, at most 1.

**Past the review.** ACME-3 11:30 to 12:10, ACME-4 13:10 to 13:50, ACME-1 13:20 to 14:00, ACME-2
14:20 to 15:00, 40 minutes each, none with a slot: `pastReviewNotHolding` 160 minutes, 2.6666…
hours, an average of 0.380952…, and 2 at once from 13:20 to 13:50; `pastReviewHolding` 0.

**Priority taken.** 09:10 to 09:40: ACME-3 (`a2`) waits, ACME-1 (`a0`) and ACME-2 (`a1`) hold, a
share of 0. 09:15 to 09:45: ACME-4 (`a3`) waits too, under holders ranked above it (ACME-1 and ACME-2,
then ACME-2 and ACME-3 from 09:40), a share of 0. 12:10 to 13:10: ACME-2 (`a1`) waits while ACME-4
(`a3`) and ACME-1 (`a0`) hold: of the two pairs, ACME-4 ranks below ACME-2 and ACME-1 above, a share
of 1/2. The average is 0.5 × 1/7 = 0.071428….

**Loops.** None.

**Waits on another issue.** ACME-1's wait on ACME-3, 09:40 to 12:10 (2.5 hours), and ACME-2's, 09:45
to 12:10 (2.41666… hours), each ended by ACME-3 reaching `done`: `blocker_done`, count 2, 4.91666…
hours.

**Assumptions.** 1; 2; 4 (the two link waits); 9; 10 (two links); 11 (ACME-1's slot, freed at 09:40,
to ACME-3; ACME-2's, freed at 09:45, to ACME-4; ACME-4's, freed at 13:10, to ACME-2). `assumptions`
is [1, 2, 4, 9, 10, 11]; `scenario` is `none`.

## The scorecard under `cover`, worked by hand

**Slots.** ACME-1 09:00 to 09:40 (40 minutes) and 12:10 to 14:00 (110); ACME-2 09:05 to 14:00 (295);
ACME-3 on ACME-1's slot 09:40 to 12:10 (150); ACME-4 14:00 to 16:00 (120). 5 slots given on
2001-03-05 (ACME-1, ACME-2, ACME-3's cover, ACME-1's back, ACME-4); `firstSlot` ACME-1 09:00, ACME-2
09:05, ACME-3 09:40, ACME-4 14:00. The waits: 0, 0, ACME-3 from 09:10 to 09:40 (30), ACME-1 at 12:10
0, ACME-4 from 09:15 to 14:00 (285). Sorted: 0, 0, 0, 30, 285; the median is 0.

**Slot-hours.** ACME-1 09:00 to 09:40: 40 in a turn; 12:10 to 14:00: six turns back to back, 110.
ACME-2 09:05 to 10:05 in a turn (architect, planner, 60), 10:05 to 12:10 with none (125, its
implementing start waiting on ACME-3), 12:10 to 14:00 in a turn (110). ACME-3 09:40 to 12:10, 150 in
a turn. ACME-4 14:00 to 16:00: architect, planner, implementer, 120 in a turn. `slotHoursInTurn` 40 +
110 + 60 + 110 + 150 + 120 = 590 minutes, 9.8333… hours; `slotHoursNoTurn` 125 minutes, 2.08333…
hours.

**In flight.** ACME-1 09:00 to 14:00 (300), ACME-2 09:05 to 14:00 (295), ACME-3 09:40 to 12:10
(150), ACME-4 14:00 to 16:00 (120): 865 minutes, 14.41666… hours, an average of 2.059523…, and 3 at
once from 09:40 to 12:10.

**Done and work left.** ACME-3 12:10, ACME-1 and ACME-2 14:00 (`done` 3); ACME-4 in flight at 16:00
(`workLeft` 1).

**Started work without a slot.** ACME-1, covered, holds no slot from 09:40 to 12:10: reading (b) 2.5
hours, an average of 0.357142…, at most 1. It waits on ACME-3 all that time and takes its slot back
in the pass that ends the wait: reading (a) 0. ACME-2 keeps its own slot.

**Past the review.** ACME-3 11:30 to 12:10, ACME-1 and ACME-2 13:20 to 14:00, each holding its slot:
`pastReviewHolding` 120 minutes, 2 hours, an average of 0.285714…, and 2 at once from 13:20 to 14:00;
`pastReviewNotHolding` 0.

**Priority taken.** ACME-3 (`a2`) waits from 09:10 to 09:40 under ACME-1 and ACME-2; ACME-4 (`a3`)
from 09:15 to 14:00 under holders all ranked above it (ACME-1 and ACME-2; ACME-2 and ACME-3; ACME-1
and ACME-2): every share is 0.

**Loops.** None.

**Waits on another issue.** The same two waits, ended by ACME-3's `done` at 12:10: `blocker_done`,
count 2, 4.91666… hours.

**Assumptions.** 1; 2; 4; 9; 10; 11 (ACME-3's slot comes back to ACME-1 at ACME-3's `done`, and the
slots freed at 14:00 go to ACME-4, each in that pass); 14 (ACME-3's cover); 15 (ACME-4 in flight at
16:00). `assumptions` is [1, 2, 4, 9, 10, 11, 14, 15]; `scenario` is `none`.
