# The window `phase-goes-on-during-link-wait`

A recorded window of project ACME, written by hand by the conventions of `../README.md`: six hours,
from 2001-03-05T09:00Z to 15:00Z, an admission cap of 1, two issues, recorded under the rule
`main`. It holds one fact of check k1: a phase under way goes on while its issue waits on a link,
and only its next start waits for the wait to end (spec R2). ACME-1's implementing phase goes on
through its wait on ACME-2, and its testing start waits from 11:30, when implementing ends, to
12:30, when ACME-2 is done. `expect.json` holds the scorecard of a replay under `main` on the main
branch, worked by hand below. Nothing in the window is drawn (no experiment, no conflict recorded,
no unstarted issue of Legion's), so every seed gives it.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root handed to Legion that runs every phase to `done` | `a0` |
| ACME-2 | an issue Legion does not work (no `legion` label), in `backlog` until a person closes it | `a1` |

## What happens

The record, as `log.jsonl` and `events/` hold it (the daemon's own status writes, by the session
`legion-daemon:ACME`, are left out of this table):

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots; ACME-2 filed, `backlog`, no label | the mirror records ACME-2 |
| 09:05 | ACME-1 filed, `todo`, labelled `legion` | promotes it: slot 1 of 1, architect turn 09:05 to 09:30 |
| 09:30 | ACME-1's design registered (no gated step: the gate opens at once) | to planning by `gate_opened`; planner turn 09:30 to 10:00 |
| 10:00 | `planner_completed` | to implementing; implementer turn 10:00 to 11:30 |
| 10:30 | a person updates ACME-1: `blocked_by: [ACME-2]` (`events/ACME-1.jsonl`, seq 3) | ACME-1 stands in implementing, which keeps a slot, and waits on ACME-2, which is not `done` (`blockers.waitsOf`): the pass gives its slot back (`admission: slot released`, 0 of 1). The implementer's turn goes on. |
| 11:30 | `implementation_ready` | to testing. The tester's start is owed with `needsSlot`; ACME-1 holds no slot and still waits on ACME-2, so the start does not run. |
| 12:30 | a person closes ACME-2 (`done`, `events/ACME-2.jsonl`, seq 2) | ACME-1's wait ends: `admission gives a slot back to an issue whose wait ended` (testing), slot 1 of 1; its tester starts, turn 12:30 to 13:00 |
| 13:00 | `tester_passed` | to reviewing; reviewer turn 13:00 to 13:30 |
| 13:30 | `review_approved` | to accepting; its slot goes back (accepting keeps none); architect turn 13:30 to 13:45 |
| 13:45 | `sign_off` | to retro; implementer turn 13:45 to 14:00 |
| 14:00 | `retro_completed` | to merging; merger turn 14:00 to 14:30 |
| 14:30 | `pull_request_merged` | done; ACME-1 leaves the workflow and its tree lingers to 16:30 |

The work segments, each from its worker start (the later of the phase change and the slot taken)
to the move that ends it:

| phase | phase change | slot | worker start | move | length |
| --- | --- | --- | --- | --- | --- |
| admitted | 09:05 (filed) | 09:05 | 09:05 | 09:30 | 25 min |
| planning | 09:30 | held | 09:30 | 10:00 | 30 min |
| implementing | 10:00 | held | 10:00 | 11:30 | 90 min |
| testing | 11:30 | 12:30 | 12:30 | 13:00 | 30 min |
| reviewing | 13:00 | held | 13:00 | 13:30 | 30 min |
| accepting | 13:30 | none kept | 13:30 | 13:45 | 15 min |
| retro | 13:45 | none kept | 13:45 | 14:00 | 15 min |
| merging | 14:00 | none kept | 14:00 | 14:30 | 30 min |

## The replay under `main`

The recorded rule, so each pass decides as the record's did. The link at 10:30 is a person's
event at its recorded time. ACME-2's close is the close of an outside blocker (Legion does not
work it), which the main branch's `Timing = offset` places at its recorded offset from the start of
the wait it ends: the wait starts at 10:30 in the record and in the replay, and the close came 120
minutes into it, so it comes at 12:30. Every segment starts at its recorded worker start with the
record's one issue in flight (n = m = 1), so each keeps its length: ACME-1 merges at 14:30.

## A tool that stops the phase

A tool that stopped ACME-1's implementing phase during its wait would stop it at 10:30 with 60 of
its 90 minutes left, start it again with the slot at 12:30 and end it at 13:30; testing would run
13:30 to 14:00, reviewing 14:00 to 14:30, accepting 14:30 to 14:45, retro 14:45 to 15:00 and
merging from 15:00, past the window's end. It would give `done` 0, no `doneAt`, `workLeft` 1, and
it fails against `expect.json`, which says ACME-1 is done at 14:30.

## The scorecard under `main`, worked by hand

The span is 6 hours; an average is the hours divided by 6.

**Slots.** ACME-1 holds one from 09:05 to 10:30 (85 minutes) and from 12:30 to 13:30 (60
minutes). Two slots are given out on 2001-03-05, at 09:05 and 12:30 (`slotsGivenByDay`), and
ACME-1's first is at 09:05 (`firstSlot`). ACME-1 took the first in the pass of its own filing and
the second in the pass that ended its wait, so both waits for a slot are 0 and their median is 0
(`medianSlotWaitMinutes`).

**Slot-hours with a worker in a turn, and with none.** 09:05 to 10:30: architect 09:05 to 09:30,
planner 09:30 to 10:00, implementer 10:00 to 10:30, 85 minutes in a turn. 12:30 to 13:30: tester
12:30 to 13:00, reviewer 13:00 to 13:30, 60 minutes in a turn. So `slotHoursInTurn` is 145
minutes, 145/60 = 2.416666… hours, and `slotHoursNoTurn` is 0. The implementer's turn from 10:30
to 11:30 runs with no slot and is in neither.

**In flight.** ACME-1 from its promotion at 09:05 to its `done` at 14:30: 325 minutes, 5.416666…
hours, an average of 5.416666…/6 = 0.902777…, at most 1 (`inFlight`). ACME-2 is never started.

**Done and work left.** ACME-1 reaches `done` at 14:30 (`done` 1, `doneAt`); nothing is left at
15:00 (`workLeft` 0); the start state is empty (`unplaced` 0).

**Started work without a slot.** From 10:30 to 12:30 ACME-1 is a returned member of the line, given
back for its wait (its release mark), holding no slot: reading (b), `startedWithoutSlot`, is 2
hours, an average of 2/6 = 0.333…, at most 1. `admission.blocked` names ACME-1 for all of it (it
waits on ACME-2), and the pass that ends the wait gives it its slot, so reading (a),
`waitOnlyForSlot`, is 0.

**Past the review.** ACME-1 stands in accepting, retro and merging from 13:30 to 14:30 with no slot:
`pastReviewNotHolding` is 1 hour, an average of 1/6 = 0.1666…, at most 1; `pastReviewHolding` is
0.

**Priority taken.** No issue ever waits for a slot alone (ACME-1's wait holds it back until the
pass that gives it one), so there is never a pair and `priorityTaken` is 0.

**Loops.** No move back (`loops` is empty).

**Waits on another issue.** ACME-1's wait on ACME-2, from 10:30 to 12:30, ended by ACME-2 reaching
`done`: `waits` holds `blocker_done` with a count of 1 and 2 hours.

**Assumptions.** By the rules of `../README.md`: 1 (every run); 2 (ACME-1 stands past its review);
4 (ACME-1 waits on ACME-2); 9 (the run gives out slots); 10 (the window holds a link). No slot is
freed and given out in one pass (11), and nothing is left at 15:00 (15). `assumptions` is
[1, 2, 4, 9, 10], and `scenario` is `none`, the main branch's.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all five signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
