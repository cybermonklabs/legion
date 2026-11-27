# The window `clock-far-from-now`

A recorded window of project ACME, written by hand by the conventions of `../README.md`, with every time in 2001:
six hours, from 2001-03-05T09:00Z to 15:00Z, four issues and an admission cap of 2.
`TestReplayWritesNoWallClockTime` (`../../../clock_test.go`) replays it under the rule `main` and reads every time column of the replay's database.
A time the replay took from the wall clock, or from Postgres's `now()`, is twenty-five years away from the window, so no value can fall inside the window's span plus a day by chance.

`expect.json` is the scorecard of a replay under `main` on the main branch, worked by hand below.
Nothing in the window is drawn (no experiment is declared, no conflict is recorded and no issue
outside the start state is left unstarted under the scenario `none`), so every seed gives it.

## The issues

| issue | what it is | rank |
| --- | --- | --- |
| ACME-1 | a root handed to Legion, whose implementer fails; a person sets it to `todo` and links it to ACME-4 in one update | `a0` |
| ACME-2 | a root handed to Legion that runs every phase to `done` | `a1` |
| ACME-3 | a root handed to Legion while the cap is full, which waits in the line and is promoted at 11:00 | `a2` |
| ACME-4 | an issue Legion is not given (no `legion` label), in `backlog` from its filing at 10:50 | `a3` |

`log.jsonl` holds the recorded daemon's lines as its JSON handler writes them (`time`, `level`, `msg`, then the attributes, a machine's `claim` first), with the msg strings and attribute keys of the code:
`daemon: opening the store; …` (`internal/daemon/daemon.go`) at the boot, `admission: slot taken|released` (`admit.(*Admission).logSlot`), `workflow: phase changed` (`workflow.(*Engine).transition`), `workflow: design gate registered` (`workflow/gate.go`), `workflow: the root of this tree takes its slot and starts its own first phase` (`workflow.(*Engine).StartFirstPhase`), `workflow: a held phase is released` (`workflow.(*Engine).retryPhase`), and the `supervise:` lines of `supervise/machine.go` (`launched`, `a turn started`, `a turn ended`, `claim failed`).
A claim's token is `legion-acme-<key>-<role>`, lower-cased.
The `delivery` of a turn names the record's own outbox rows; a replay's rows have ids of their own.

## What happens

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots | |
| 09:05 | ACME-1 created, `todo`, labelled `legion` | promotes it: opens its tree, gives it slot 1 of 2, starts its architect (turn 09:05 to 09:25) |
| 09:10 | ACME-2 created, `todo`, labelled `legion` | promotes it: slot 2 of 2, architect (turn 09:10 to 09:40) |
| 09:25 | ACME-1's design is registered (the deployment gates no step, so the gate opens at once) | ACME-1 admitted to planning by `gate_opened`; planner turn 09:25 to 10:05 |
| 09:30 | ACME-3 created, `todo`, labelled `legion` | the cap is full: ACME-3 waits in the line's starting part |
| 09:40 | ACME-2's design is registered | ACME-2 to planning; planner turn 09:40 to 10:20 |
| 10:05 | ACME-1 `planner_completed` | to implementing; implementer turn 10:05 to 10:35 |
| 10:20 | ACME-2 `planner_completed` | to implementing; implementer turn 10:20 to 11:30 |
| 10:35 | ACME-1's implementer claim fails (`supervise: claim failed`) | ACME-1 held, from implementing; it keeps its slot (`held` keeps one under `main`) |
| 10:50 | ACME-4 created, `backlog`, no label | the mirror records it; Legion does nothing with it |
| 11:00 | ACME-1 updated by the person: `todo`, `blocked_by: [ACME-4]` | the one pass below: ACME-1's implementer is owed a start, ACME-1 gives its slot back for the wait, ACME-3 takes it |
| 11:30 | ACME-2 `implementation_ready` | to testing; tester turn 11:30 to 12:00 |
| 11:40 | ACME-3's design is registered | ACME-3 to planning; planner turn 11:40 to 14:40 |
| 12:00 | ACME-2 `tester_passed` | to reviewing; reviewer turn 12:00 to 12:30 |
| 12:30 | ACME-2 `review_approved` | to accepting; its slot goes back (accepting keeps none); architect turn 12:30 to 12:45 |
| 12:45 | ACME-2 `sign_off` | to retro; implementer turn 12:45 to 13:00 |
| 13:00 | ACME-2 `retro_completed` | to merging; merger turn 13:00 to 13:30 |
| 13:30 | ACME-2 `pull_request_merged` | done |
| 15:00 | the window ends | ACME-1 implementing with no slot, waiting on ACME-4, its start owed; ACME-3 planning on slot 2 |

## The writes the clock test reads

A replay of this window makes its pass write each kind of time the test needs:

- an outbox row, a slot, an issue's `moved_at` and a `processed_events` row: every promotion
  (09:05, 09:10, 11:00) and every fact;

- a tree lifecycle's epoch: each queued root's promotion opens its tree
  (`treelifecycle.Open`, through `record.Store.OpenTreeLifecycle` from
  `admit.(*Admission).promoteHolds`), ACME-3's at 11:00 among them;

- a release mark and an owed start, in one pass, at 11:00 (next section).

### The release mark and the owed start of one pass

Event 107 (`events/ACME-1.jsonl`, seq 3) is one fact, applied in one transaction through the
chain `admit.Handlers` (`internal/admit/handlers.go`), in its order:

1. `blockers.(*Mirror)` (`internal/blockers/mirror.go`) writes ACME-1's `blocked_by: [ACME-4]`
   into `dispatch_issues`.

2. `workflow.(*Engine).dispatchIssue` (`internal/workflow/engine.go`):
   the status `todo` is new, the person wrote it, and ACME-1 is a root held from implementing whose tree does not linger, so it calls `(*Engine).releaseHeldRoot`, then `(*Engine).retryPhase` (logging `workflow: a held phase is released`), then `(*Engine).restart` and `(*Engine).started`, then `StartOrOwe` (`internal/workflow/effects.go`), which marks the implementer's start `needsSlot` (implementing keeps a slot) and writes it with `record.Store.EnqueueStart` (`internal/record/store.go`).
   That is the owed start, `outbox.created_at`, written **first**.
   ACME-1 still holds its slot at this point, so the row is claimable until step 3 commits.

3. `admit.(*Admission).Apply`, then `promote` and `promoteHolds` (`internal/admit/admit.go`):
   `blockers.JudgeLine` finds ACME-1 standing in implementing, which keeps a slot, waiting on ACME-4, which is not done (`blockers.waitsOf`), so `(*Admission).releaseSlotsForWaits` calls `record.Store.ReleaseSlotForWait(ACME-1, a.now())`, which deletes the slot row and writes `issues.released_for_wait_at`.
   That is the release mark, written **second**.
   The same pass then gives the freed slot to ACME-3, the first member of the line that waits only for the cap (ACME-1 is passed over: it waits on ACME-4).

From then to the window's end ACME-1 holds no slot and ACME-4 stays in backlog, so the start stays owed (unclaimable without a slot row) and the mark stays standing:
the test reads both from the replay's database once the run ends.
Written in that order in one pass, they sort in the waiting line (`record.Waiting`, which orders a mark at its time with id 0 and an owed start at its `created_at` and id) as written only when `created_at` is before `released_for_wait_at`, so the test requires exactly that.

## The settings

- `gateSteps` is empty: the deployment gates no design step, so each root's registration opens
  its gate at once, and no Dispatch artifact event is needed.

- `linger` is 2h, so ACME-2's linger ends at 15:30, inside the window's span plus a day: every
  time a correct replay writes is inside it.

- `workerSilence` and `workerBackground` are 4h: the log records no tool step of any worker, so
  no turn of the window may be read as a silent worker's.

- No experiment is declared, so no draw is made and no assignment is written.

- The window names no production check: a replay of it opens none, whose bound would be two
  weeks after ACME-2's merge.

- `release` is `fixture` and `collector` is the commit the rule `main` names: the window was
  written by hand, not collected.

## The scorecard, worked by hand

The span is 6 hours.
An average is the hours divided by 6.

The slots: ACME-1 holds one from 09:05 to 11:00 (115 minutes), ACME-2 from 09:10 to 12:30 (200 minutes), ACME-3 from 11:00 to 15:00 (240 minutes):
3 slots given out on 2001-03-05, at 09:05, 09:10 and 11:00 (`firstSlot`, `slotsGivenByDay`).
ACME-1 and ACME-2 took theirs in the pass that recorded them, and ACME-3 waited from 09:30 to 11:00, so the waits for a slot are 0, 0 and 90 minutes, with a median of 0 (`medianSlotWaitMinutes`).

Slot-hours with a worker in a turn, and with none:

| holder | in a turn | in none |
| --- | --- | --- |
| ACME-1 | architect 20, planner 40, implementer 30 (its turn ends with the failed claim at 10:35): 90 minutes | held, 10:35 to 11:00: 25 minutes |
| ACME-2 | architect 30, planner 40, implementer 70, tester 30, reviewer 30: 200 minutes | none |
| ACME-3 | architect 40, planner 180: 220 minutes | 14:40 to 15:00: 20 minutes |

So `slotHoursInTurn` is 510 minutes, 8.5 hours, and `slotHoursNoTurn` 45 minutes, 0.75 hours.

The issues in flight, from the promotion that started each to its `done`:
ACME-1 from 09:05 to the end (355 minutes), ACME-2 from 09:10 to 13:30 (260 minutes), ACME-3 from 11:00 to the end (240 minutes):
14.25 hours, an average of 2.375, and 3 at once from 11:00 to 13:30 (`inFlight`).
ACME-2 reaches done at 13:30 (`done` 1, `doneAt`), and ACME-1 and ACME-3 are started and not done at 15:00 (`workLeft` 2).
The start state at 09:00 is empty, so no issue is unplaced.

Started work without a slot: from 11:00 to 15:00 ACME-1 is a returned member of the line (it has started and waits on ACME-4) holding no slot, and nothing else is:
reading (b), `startedWithoutSlot`, is 4 hours, an average of 4/6, at most 1.
Reading (a), `waitOnlyForSlot`, leaves out a member that `admission.blocked` names, and ACME-1 is blocked by ACME-4 for all of it, so it is 0.

Past the review: ACME-2 stands in accepting, retro and merging from 12:30 to 13:30 holding no
slot (`pastReviewNotHolding`: 1 hour, an average of 1/6, at most 1), and no issue past its
review holds one (`pastReviewHolding` 0).

Priority taken: the one issue that waits for a slot alone is ACME-3, from 09:30 to 11:00, while ACME-1 (`a0`) and ACME-2 (`a1`) hold the two slots.
Both rank above it (`a2`), so no pair has the holder below the waiter, and the share is 0 (`priorityTaken`).
From 11:00 ACME-1 waits on ACME-4 and is no issue waiting for a slot.

Loops: no send-back of any cause (`loops` is empty).
Waits on another issue: ACME-1's on ACME-4, from 11:00, which nothing ends before 15:00; it is counted under `open` with its 4 hours (`waits`).

`assumptions` names the assumptions of the spec these numbers rest on: 1 (every segment keeps
its recorded length, f being 1), 2 (ACME-2's stretch past its review keeps its length and its
outcome), 4 (ACME-1's wait on ACME-4 ends at its recorded time, here never), 9 (returned work
first, then rank, which puts ACME-3 behind no one once ACME-1 waits), 10 (the link from ACME-1
to ACME-4 has the lifetime its event gives it), 11 (ACME-1's slot is given to ACME-3 in the pass
that freed it) and 15 (ACME-1 and ACME-3 are work left, not issues that would never finish).
`scenario` is `none`, the main branch's.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `main` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all five signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
