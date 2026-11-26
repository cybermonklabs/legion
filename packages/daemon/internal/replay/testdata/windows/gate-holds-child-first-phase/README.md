# The window `gate-holds-child-first-phase`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: six hours,
from 2001-03-05T09:00Z to 15:00Z, an admission cap of 1, two issues, recorded under the rule
`main`, with the deployment gating the design step `spec`. It holds one fact of check k1: a design
gate holds a child's first phase (spec R2: a design gate applies at the time the window records).
The child ACME-2 is recorded at 09:20, but its tree's gate stays closed until a person approves the
root's spec at 10:00, and only then does its first phase start. `expect.json` holds the scorecard
of a replay under `main` on the main branch, worked by hand below; nothing is drawn, so every seed
gives it.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root handed to Legion; its spec needs a person's approval | `a0` |
| ACME-2 | a child of ACME-1, filed `todo` by ACME-1's architect, so recorded at once in ACME-1's tree | `a1` (its root's `a0` places it) |

## What happens

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots | |
| 09:05 | ACME-1 filed, `todo`, labelled `legion` | promotes it: slot 1 of 1, architect turn 09:05 to 09:35 |
| 09:20 | ACME-2 filed under ACME-1 by its architect's session, `todo` (`events/ACME-2.jsonl`, seq 1) | records ACME-2 at admitted in ACME-1's live tree (`workflow.recordChildUnderLiveTree`). The tree's gate has no row yet, so it is not open (`record.TreeGate.Open`): the line holds ACME-2 (the held word `design_gate`). |
| 09:30 | the architect writes version 1 of `spec.md` (`artifact.version`, seq 3) | |
| 09:35 | the architect's turn ends; it registers version 1, not approved (`workflow: design gate registered`, `open` false, `gated_steps` [spec]) | the gate stays closed. ACME-1 stands at admitted with a gate that never opened, its architect at work (`record.AtWork`), so it keeps its slot and waits on nothing. No ask is open, so no wait on a person begins. |
| 10:00 | a person approves version 1 (`artifact.approved`, seq 4) | `workflow: design gate opened`. In the same pass ACME-1 stands at admitted under an open gate with an open child, ACME-2: its children wait begins (`admission: an issue waits for the Dispatch children under it`) and its slot goes back (0 of 1); the line's first member is ACME-2, whose first phase can start now: slot 1 of 1, admitted to planning by `gate_opened` (`workflow: this child names no issue it waits on any longer, so it starts`), its own architect launched and its planner's turn 10:00 to 10:30 |
| 10:30 | ACME-2 `planner_completed` | implementing; turn 10:30 to 11:30 |
| 11:30 | ACME-2 `implementation_ready` | testing; turn 11:30 to 12:00 |
| 12:00 | ACME-2 `tester_passed` | reviewing; turn 12:00 to 12:30 |
| 12:30 | ACME-2 `review_approved` | accepting; its slot goes back (0 of 1); ACME-2's architect turn 12:30 to 12:45. ACME-1 still waits on ACME-2, so nothing takes the slot. |
| 12:45 | ACME-2 `sign_off` | retro; implementer turn 12:45 to 13:00 |
| 13:00 | ACME-2 `retro_completed` | merging; merger turn 13:00 to 13:30 |
| 13:30 | ACME-2 `pull_request_merged` | ACME-2 done and leaves the workflow; with its `done` (seq 6) ACME-1's children wait ends: `admission gives a slot back to an issue whose wait ended` (admitted), slot 1 of 1, and in the same pass its own first phase starts (`workflow: the root of this tree takes its slot and starts its own first phase`); planner turn 13:30 to 14:00 |
| 14:00 | ACME-1 `planner_completed` | implementing; turn 14:00 to past 15:00 |

The work segments: ACME-1 admitted 09:05 to 09:35 (30 min), planning 13:30 to 14:00 (30 min),
implementing from 14:00, unended at 15:00; ACME-2 planning 10:00 to 10:30 (30), implementing 10:30
to 11:30 (60), testing 11:30 to 12:00 (30), reviewing 12:00 to 12:30 (30), accepting 12:30 to 12:45
(15), retro 12:45 to 13:00 (15), merging 13:00 to 13:30 (30).

## The replay under `main`

The recorded rule. The approval is a person's review of a spec version, which the main branch's
`Answer = offset` places at its recorded offset from the registration of the version reviewed: the
registration falls at 09:35 in the replay too (the end of ACME-1's architect segment, 30 minutes
after its 09:05 start), and the approval came 25 minutes after it, at 10:00. ACME-2's filing comes
at its recorded offset in that segment, 15 minutes after its start, at 09:20. So every pass falls
at its recorded time, every segment starts at its recorded worker start with n = m, and the replay
is the record. A replay that started ACME-2's first phase before the gate opened, at its filing,
would give a slot to ACME-2 at 09:20 that the cap of 1 does not have, or take ACME-1's: either way
`firstSlot` of ACME-2 would not be 10:00.

## The scorecard under `main`, worked by hand

The span is 6 hours.

**Slots.** ACME-1 holds one from 09:05 to 10:00 (55 minutes), ACME-2 from 10:00 to 12:30 (150) and
ACME-1 again from 13:30 to 15:00 (90): 3 slots given out on 2001-03-05, at 09:05, 10:00 and 13:30
(`slotsGivenByDay`); `firstSlot` ACME-1 09:05, ACME-2 10:00. Each was taken in the pass where its
issue began to wait for it — ACME-1 at its filing, ACME-2 at the gate's opening (before it the gate
held it, which is no wait for a slot), ACME-1 at the end of its children wait — so the waits are
0, 0 and 0 and their median is 0.

**Slot-hours.** ACME-1 09:05 to 10:00: its architect's turn 09:05 to 09:35 (30 minutes), then no
turn to 10:00 (25 minutes). ACME-2 10:00 to 12:30: planner, implementer, tester and reviewer back to
back, 150 minutes in a turn. ACME-1 13:30 to 15:00: planner 13:30 to 14:00 and implementer 14:00 to
15:00, 90 minutes in a turn. `slotHoursInTurn` is 30 + 150 + 90 = 270 minutes, 4.5 hours;
`slotHoursNoTurn` is 25 minutes, 0.41666… hours.

**In flight.** ACME-1 from 09:05 to 15:00 (355 minutes; it gave its slot back for its wait but
stays in flight), ACME-2 from 10:00 to 13:30 (210 minutes): 565 minutes, 9.41666… hours, an average
of 9.41666…/6 = 1.569444…, and 2 at once from 10:00 to 13:30 (`inFlight`).

**Done and work left.** ACME-2 is done at 13:30 (`done` 1, `doneAt`); ACME-1 is in flight at 15:00
(`workLeft` 1).

**Started work without a slot.** ACME-1 is a returned member of the line with no slot from 10:00 to
13:30, given back for its children wait: reading (b) is 3.5 hours, an average of 3.5/6 = 0.58333…,
at most 1. `admission.blocked` names it all that time (it waits on ACME-2), and the pass that ends
the wait gives it its slot, so reading (a) is 0.

**Past the review.** ACME-2 stands in accepting, retro and merging from 12:30 to 13:30 with no slot:
`pastReviewNotHolding` 1 hour, an average of 0.1666…, at most 1; `pastReviewHolding` 0.

**Priority taken.** No issue ever waits for a slot alone: ACME-2 is held by the gate until the pass
that gives it its slot, and ACME-1 is held back by its wait. `priorityTaken` is 0.

**Loops.** None.

**Waits on another issue.** ACME-1 waits on its child from the gate's opening at 10:00 (before it,
a root whose gate never opened waits on nothing) to ACME-2's `done` at 13:30: `waits` holds
`child_done` with a count of 1 and 3.5 hours. ACME-2's hold by the gate, 09:20 to 10:00, is a hold
of a start, not a wait on another issue.

**Assumptions.** 1; 2 (ACME-2 past its review); 4 (ACME-1's children wait); 5 (a person's approval
opens the gate); 7 (the gate holds ACME-2's first start); 9 (slots given out); 10 (a child); 11
(ACME-1's slot, freed at 10:00, goes to ACME-2 in that pass); 15 (ACME-1 in flight at 15:00); 17
(ACME-2 filed after ACME-1 started). `assumptions` is [1, 2, 4, 5, 7, 9, 10, 11, 15, 17];
`scenario` is `none`.

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
