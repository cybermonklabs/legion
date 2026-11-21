# The fixture windows of `legion replay`

Each directory here is one recorded window of project ACME, in the format of `window.go`, written
by hand around one fact that check k1 of the tool's verification names. `TestFixtureWindows`
(`../../fixtures_test.go`) reads this directory, replays every window under every rule its
`expect.json` names through `Run`, and fails naming the window, the rule and each measure that
differs from the value worked out by hand. `TestTheFixtureWindowsCoverK1` holds k1's list of facts
and fails for a fact with no window here. This file gives the conventions every window is written
and worked by, once: the trace builder (plan t7), the replay (t9) and the measures (t10) must
meet them, and each window's `README.md` works its values out by them, step by step, from its own
files.

| directory | the fact | rules |
| --- | --- | --- |
| `clock-far-from-now` | every time the replay writes is the simulated clock's | `main` |
| `phase-goes-on-during-link-wait` | a phase goes on while its issue waits on a link; only its next start waits | `main` |
| `gate-holds-child-first-phase` | a design gate holds a child's first phase until a person approves | `main` |
| `link-added-and-removed` | a link added and taken off inside the window, each at its time | `main` |
| `child-moved-out` | a child moved out from under its parent ends the parent's wait | `main` |
| `child-filed-after-parent-start` | a child's filing moves with its parent's start | `keep-past-review`, `main` |
| `conflict-from-fitted-rate` | a conflict drawn from the window's fitted rate, on each conflict path | `infra-w1`, `main` |
| `review-and-test-kept` | a review that asks for changes and a failed test keep their count and length | `main` |
| `reopen-works-again` | an issue that reopens keeps its later work | `main` |
| `send-back-at-full-cap` | a send-back from accepting at a full cap waits for a slot, or keeps one | `main`, `keep-past-review`, `cover` |
| `two-waiters-one-blocker` | two started issues wait on one blocker | `main`, `cover` |
| `priority-inversion` | a holder ranked below an issue that waits for a slot | `main` |
| `never-starts` | an issue the record never started, under each scenario | `main` |
| `forty-two-more-in-flight` | a segment that starts with 42 more issues in flight grows by f | `keep-past-review`, `main` |
| `every-trigger` | a root and two children through every trigger keep their recorded moves | `main` |

## What a directory holds

- `window.json`, `log.jsonl` and `events/<KEY>.jsonl` (and `merges/<owner>__<repo>.jsonl` where a
  window draws conflicts), in the format of `window.go`: `ReadWindow` checks each input against the
  sha256 `files` records. After an edit of an input, write its new hash into `files`
  (`sha256sum log.jsonl events/*.jsonl merges/*.jsonl`); `TestTheFixtureWindowsCoverK1` reads every
  listed window with `ReadWindow`, so a hash that differs fails it.
- `README.md`: the window's issues, what happens, what changes under each rule it names, and every
  expected value worked out by hand from the window's files.
- `expect.json`, with these keys:
  - `rules`: `{"<rule>": <Scorecard>}`, one `Scorecard` (`scorecard.go`, its JSON names) for each
    rule the window is replayed under. The run takes the seed and the slowdown factor of its own
    `Scorecard` (`seed`, `slowdown`), so `forty-two-more-in-flight` runs at f = 1.5.
  - `branch`: the branch the rules run on, `main` (the main branch of design section 7) where the
    key is left out. A branch other than `main` is named `main+<knob>=<value>`, by the knob's JSON
    name in `replay.Branch`.
  - `scenarios` (`never-starts` alone, in place of `rules`): `{"<scenario>": {"<rule>":
    <Scorecard>}}`, one set for each value of `NeverStarted`; the scenario `none` runs on `main`,
    the others on `main+neverStarted=<scenario>`.
  - `moves` (`every-trigger` alone): `{"<KEY>": [{"from", "to", "trigger"}, …]}`, each issue's
    recorded moves in order. A replay under the window's recorded rule must write the same
    `workflow: phase changed` lines in its own log, issue by issue, which a move the replay refuses
    (design section 10, item 3) breaks.

The values of `expect.json` are those of the snapshot timeline of design section 10, item 1 (a):
the timeline the replay builds from the store as each admission pass leaves it, not the one
`BuildTrace` builds again from the replay's own log lines. The two may differ by the
reconstruction error the tool reports; these windows hold the exact one.

## How a window is written

- Every window is one day, 2001-03-05 (`clock-far-from-now` too), a few hours long, with an
  admission cap of 1 to 3 (43 in `forty-two-more-in-flight`), so each value is a sum of a few
  whole minutes. Its start state is empty: the daemon boots at the window's start (`daemon:
  opening the store; …`), nothing came before, and no issue is unplaced.
- Issue keys are `ACME-<n>`. A root handed to Legion is filed `todo` with the label `legion` by a
  person (`user:acme-person`); a child is filed under its parent by the role that files it (its
  claim token as a session) with no label, `todo` (recorded at once under a live tree) or `backlog`
  (not released, which Dispatch still counts as an open child). An issue Legion does not work has
  no label. Ranks are short strings compared as bytes (`a0` < `a1` < `b01`).
- The record follows the daemon's own code under the rule it ran: `window.json`'s `parts` name that
  rule (`main`, `keep-past-review` or `infra-w1`), its `commit` is the fork point the windows were
  written against and its `release` is `fixture`. The log holds each line kind of the code the
  window's facts make, with the code's msgs and attribute keys (`window_builder_test.go` writes the
  same shapes): the admission pass's slot lines (`admission: slot taken|released`, the in-transaction
  `admission gives a slot back to …` and `admission: an issue waits for the Dispatch children under
  it`), the engine's `workflow:` lines, and the supervisor's `supervise: launched`, `a turn
  started` and `a turn ended`. Within one instant, a pass's in-transaction lines come before its
  after-commit lines. `window.json`'s `lines` lists every msg the log holds; a kind left out (the
  per-pass `admission waits for what this issue waits on`) is a kind the window does not hold.
- The daemon's own status writes are events of the session `legion-daemon:ACME`. These windows
  write each one at the instant of the move that queued it; `clock-far-from-now` writes them a
  second later. A replay leaves them out and applies the echo of its own status write at the
  instant of the fact that queued it, which matters where a Legion issue's `done` ends another
  issue's wait (`two-waiters-one-blocker`).
- Settings: `gateSteps` empty (the registration opens the gate at once; `gate-holds-child-first-phase`
  gates `spec`), `linger` 2 h, `tickInterval` 1 h, `reviewRoundCap` 3, `workerSilence` and
  `workerBackground` 4 h, no experiment. A tick changes nothing in any window: no blocker is in
  another project and every wait ends on an event.

## Work segments and their replayed length

- A work segment is one visit of an issue to a phase, from the visit's worker start to the recorded
  move that ends it. The worker start is the later of the phase change into the visit and the slot
  the issue takes for it; in a phase that keeps no slot it is the phase change. A recorded segment's
  length is that move's time less the worker start: `phase-goes-on-during-link-wait`'s testing
  visit begins at the 11:30 move and its worker starts with the slot at 12:30, so the segment runs
  from 12:30 to the 13:00 move, 30 minutes.
- In a replay a segment starts at the replay's own worker start and ends at `start + length ×
  f^((n − m)/42)`, n the issues in flight in the replay at its start (after the pass that started
  it) and m the issues in flight in the record at the moment the record started it. At f = 1, or
  wherever n = m, a segment keeps its length. Its turns, and a child it files, enter at their
  recorded offsets from its start.
- A segment the record shows unended at the window's end has no known length: in a replay it does
  not end before the window does, and a turn open in it stays open (`child-filed-after-parent-start`
  under `main`).
- A sign-off the record shows at the instant a member beneath the issue leaves the workflow, after
  the architect's last turn in accepting ended earlier, is a held sign-off: its segment ends at
  that turn's end, where the replay applies `intake.SignOff`, which the engine refuses while the
  member is in the workflow, and again at each `workflow: issue left the workflow` of a member
  beneath (design section 10, item 3). `every-trigger` holds one.

## Starts and slots

- A start takes its slot in the same instant as the pass that gives it, and the worker's first turn
  begins at that instant too: a promotion, a child's first phase and a returned issue's slot are
  each one pass, and a fact's own pass gives out the slot it frees (assumption 11).
- A pass releases first and then promotes in the line's order (`record.Waiting`): the issues whose
  work came back, oldest first, then the issues whose first phase can start, by their tree root's
  rank and then their own.
- A phase under way goes on while its issue waits on another issue, and its worker's turns with
  it; only the next start waits, for the end of the wait and then for a slot. No rule these
  windows name stops the phase (`record.SlotRule.WaitStopsPhase`).
- Under `cover`, these windows read decisions-md@v1 of the tool's issue: a started issue keeps its
  slot until its work is done; an issue that a slot holder waits on starts at once on that holder's
  slot, one cover for each holder, and the holder is then a covered waiter that holds no slot until
  the cover's work is done, when it takes its slot back at once; a second waiter on an issue already
  started keeps its own slot; the line is ordered by rank; and a waiter's next start waits for its
  wait to end, slot or not.

## The knobs of design section 7

- `Timing` (assumption 4) places a link's removal, a child moved out, a gate's closing and the close
  of an outside blocker (an issue Legion does not work, of this project or another). Under `wall`
  each happens at its recorded time. Under `offset` each happens at the replay's start of the wait
  it ends plus its recorded offset from the record's start of that wait. The wait's start is its
  anchor: for a link, the later of the link's addition and the waiting issue's start; for a child
  moved out or a gate closing, the start of the parent's children wait (the later of the gate's
  opening and the child's filing); for an outside blocker's close, the start of the link wait it
  ends. An event that ends no wait in the record, or whose wait never begins in the replay, happens
  at its recorded time. A Legion blocker's own `done` is no timed event: it is the replay's own
  merge of that blocker.
- `Answer` (assumption 5) places a person's answer to an ask and a person's review of a spec
  version. Under `wall` it happens at its recorded time; under `offset` at the replay's moment of
  the question plus its recorded offset from the record's: the anchor is the ask's opening, or the
  registration of the version the person reviews, each itself at its recorded offset in its
  architect's segment.
- Under the recorded rule both values give the record's times, since every anchor falls at its
  recorded time. Every window runs `Timing` and `Answer` at `offset`.
- `AfterSendBack` runs `recorded`: each segment after a send-back keeps its recorded length from its
  own worker start. `FirstStarts` runs `recorded` and `Merges` runs `fitted`; no window here gives
  either a choice to make.
- `NeverStarted` (assumption 8) is the work of an issue handed to Legion that the record never
  started:
  - `none`: no work. When the replay starts it, it takes its slot and its architect's start opens a
    segment with no turn and no end, so it holds the slot to the window's end with no worker in a
    turn, in flight and never done.
  - `drawn`: the work of an issue drawn with the run's seed from the issues the record started in
    the window: that issue's segments from its own start, each at its recorded offset from the new
    issue's start (and unended where the drawn issue's was).
  - `drawn-done`: the same, drawn from the issues the record finished in the window.
  A draw is made certain by giving every candidate the same work over the time the drawn work runs
  in the window (`never-starts`). No scenario is a bound.

## Conflicts

- A stretch is one visit of an issue to the phases from testing to merging: from its move into
  testing to the move that ends the visit (its merge, a conflict, or another move back). M is the
  number of first-parent commits of the base branch (`merges/`) strictly after the stretch's start
  and strictly before its end; a stretch is a conflict where a recorded `conflict` move ended it.
- The chance of a conflict is `1 / (1 + exp(−(b0 + b1 × log2(1 + M))))`, b0 and b1 fitted by maximum
  likelihood on the window's own recorded stretches; `Merges = zero` sets b1 to 0. A window that
  records no conflict fits a chance of 0 for every stretch, and no conflict is drawn in it.
- In a replay each stretch draws once, at the moment its recorded end falls by its segments, with M
  counted over the replayed stretch: a draw u from the run's seeded source makes a conflict where
  u < the chance. A recorded `conflict` move is never injected: the draw makes it.
- Under `infra-w1` (`ConflictMovesBack`) a drawn conflict moves the issue back to implementing by
  `conflict`, and the loop's work is drawn from the window's recorded conflict loops: the segments
  from a recorded conflict move to the issue's return to the phase it left.
- Under `main` (`ConflictMergeForward`) a drawn conflict keeps the phase: a merge-forward segment
  follows, its length drawn from the recorded conflict loops' implementing rounds (assumption 23)
  and its implementer in a turn, and then the issue goes on with the work the record shows after the
  loop returned to the phase the conflict left. The stretch is not drawn a second time.
- `conflict-from-fitted-rate`'s stretches separate perfectly: none with M = 0 conflicted and the one
  with M = 1 did, so the maximum-likelihood chance is 0 at M = 0 and 1 at M = 1. The tool's fit must
  reach those limits to within 10⁻⁶; a draw then differs from them with a chance below 10⁻⁶ under
  any seed, which is how that window's outcome is certain by hand.

## The measures

Every measure is read over [from, to). An average over time is the integral divided by the span;
`hours` is the integral in issue-hours; `max` is the largest count at any instant. Intervals are
half-open, so an issue that leaves at the instant another enters is not counted with it.

- `source`, `rule`, `branch`, `seed`, `slowdown`, `from`, `to`: the run's own (`source` is
  `replay`). `firstDayApart` is left out: every window is shorter than a day. `unplaced` is 0.
- In flight: an issue is in flight from the pass that first gives it a slot (a root's promotion, a
  child's first phase) to the move that takes it to `done` or out of the workflow, and again from a
  reopened issue's next start. An issue that gave its slot back stays in flight; one waiting for a
  slot it never had is not. `inFlight` integrates that count; n and m of the slowdown read it.
- `done` counts the issues whose move to `done` falls in the span, each once; `doneAt` gives that
  move's time. `workLeft` counts the issues in flight at `to`. An issue that reached `done` and then
  reopened counts in both (`reopen-works-again`).
- `startedWithoutSlot` (reading (b)) counts the returned members of the waiting line that hold no
  slot: an issue whose start is owed and waits for a slot, an answered root, and an issue that gave
  its slot back for a wait on another issue (from that release, through the rest of the phase under
  way), until the pass that gives it a slot. Under `cover` a covered waiter counts.
  `waitOnlyForSlot` (reading (a)) counts the same issues while `admission.blocked` does not name
  them: from the end of the wait, or the owed start of an issue that waits on nothing, to the slot.
- `pastReviewHolding` and `pastReviewNotHolding` count the issues standing in accepting, retro or
  merging (or held from one), with a slot and without one.
- `priorityTaken`: at each instant, the pairs of a slot holder and an issue that waits for a slot
  (the plan's definition: a member of the line, in either part, that holds no slot, that no wait
  holds back and that `admission.blocked` and `admission.held` do not name), and the share of those
  pairs in which the waiting issue ranks above the holder (`record.RankLess` over the tree roots'
  ranks, then the issues' own). The share is 0 at an instant with no pair, and the measure is its
  average over the span.
- `loops`, by the word of `causes.go` (`CauseOf`): each move back to an earlier phase is one round,
  its hours from the move back to the issue's next entry into the phase the move left (or `to`).
  Under `main` a drawn conflict that keeps its phase is one round of `conflict`, its hours the
  merge-forward segment.
- `waits`, by what ended them: a wait is a stretch in which an issue in flight, standing before its
  review, waits on another issue (an unfinished `blocked_by` link, or, at admitted under an open
  gate, an open child), from the moment the wait begins to the moment nothing is left to wait on.
  The words: `blocker_done` (the last blocker reached `done`), `link_removed` (the last unfinished
  link was taken off), `child_done` (the last open child closed `done` or `icebox`),
  `child_moved_out` (the last open child moved from under the issue), `gate_closed` (a new spec
  version closed the gate), `left_workflow` (the waiting issue left), and `open` (still waiting at
  `to`). A child at admitted that a link holds has not started; its wait is the line's, not this.
- `slotHoursInTurn` and `slotHoursNoTurn`: over each slot, from the pass that gives it to its
  release (or `to`), the time a worker of the holder, of any role, is in a turn (`supervise: a turn
  started` to `a turn ended`, its claim's failure, or its issue's leave), and the rest.
- `slotsGivenByDay`: the slots given out in the span by UTC day, counting each promotion, each
  child's first phase, each slot a returned issue takes and each cover.
- `medianSlotWaitMinutes`: the median, over the slots given out in the span, of the minutes from the
  moment the issue that takes it became an issue that waits for a slot to the slot: a queued root
  from its filing (or the end of its links), a child from the moment its first phase can start, a
  returned issue from its owed start or the end of its wait. An issue that takes its slot in the
  pass where it begins to wait waited 0; an even count takes the mean of the two middle values.
- `firstSlot`: each issue's first slot in the span.
- `assumptions`: the numbers of the assumptions (`assumptions.go`) the run's measures rest on, each
  named where the run holds the fact it is about: 1 always; 2 where an issue stands past its
  review; 3 where a send-back loop runs or a conflict is drawn; 4 where an issue waits on another
  issue or a gate closes; 5 where a person's answer or review moves the run; 6 where the start state
  places an issue; 7 where a hold other than the cap (a design gate, a link on a child, an
  ancestor, a tree) holds the first start of an issue not yet started (a started issue's wait on
  another issue is 4); 8 where the window holds an issue handed to Legion that the record never
  started; 9 where the run gives out a slot, which the line's order chooses; 10 where
  the window holds a link or a child; 11 where a pass gives out a slot it freed; 13 where a loop
  moves an issue back; 14 where a cover is taken; 15 where work is left at `to`; 16 where an issue
  reopens; 17 where a child is filed after its parent started; 18 where the window records a
  conflict; 23 where a conflict is drawn under `main`; 24 where the daemon is down between two
  boots; 25 where a blocker is in another project. `clock-far-from-now` names 1, 2, 4, 9, 10, 11
  and 15 by these.
- `scenario`: the run's `NeverStarted`.
