# The fixture windows of `legion replay`

Each directory here is one recorded window of project ACME, in the format of `window.go`, written by hand around one fact: each fact that check k1 of the tool's verification names, and in `every-trigger` a root and two children through every trigger.
`TestFixtureWindows` (`../../fixtures_test.go`) reads this directory, replays every window under every rule its `expect.json` names through `Run`, and fails naming the window, the rule and each measure that differs from the value worked out by hand.
`TestTheFixtureWindowsCoverK1` holds k1's list of facts and fails for a fact with no window here.
This file gives the conventions every window is written and worked by, once: the trace builder (`BuildTrace`, `trace_build.go`), the replay (`Run`, `run.go`) and the measures (`Measure`, `measures.go`) must meet them, and each window's `README.md` works its values out by them, step by step, from its own files.

| directory | the fact | rules |
| --- | --- | --- |
| `clock-far-from-now` | every time the replay writes is the simulated clock's | `main` |
| `phase-goes-on-during-link-wait` | a phase goes on while its issue waits on a link; only its next start waits | `main` |
| `gate-holds-child-first-phase` | a design gate holds a child's first phase until a person approves | `main` |
| `link-added-and-removed` | a link added and taken off inside the window, each at its time | `main` |
| `child-moved-out` | a child moved out from under its parent ends the parent's wait | `main` |
| `child-filed-after-parent-start` | a child's filing moves with its parent's start | `keep-past-review`, `main` |
| `conflict-from-fitted-rate` | a conflict drawn from the window's fitted rate, on each conflict path | `legion-v20.42.5`, `legion-v22.14.1`, `main` |
| `review-and-test-kept` | a review that asks for changes and a failed test keep their count and length | `main` |
| `reopen-works-again` | an issue that reopens keeps its later work | `main` |
| `send-back-at-full-cap` | a send-back from accepting at a full cap waits for a slot, or keeps one | `main`, `keep-past-review`, `cover` |
| `two-waiters-one-blocker` | two started issues wait on one blocker | `main`, `cover` |
| `priority-inversion` | a holder ranked below an issue that waits for a slot | `main` |
| `never-starts` | an issue the record never started, under each scenario | `main` |
| `forty-two-more-in-flight` | a segment that starts with 42 more issues in flight grows by f | `keep-past-review`, `main` |
| `every-trigger` | a root and two children through every trigger keep their recorded moves; under `main` the root waits in `awaiting_children` for its second child | `main`, `legion-v22.14.1`, `cover` |
| `sign-off-held-by-grandchild` | a root's sign-off held behind a member of its tree that is no Dispatch child of it, which a cover holds under `cover` | `main`, `cover` |
| `one-minute-wait` | a wait on another issue that lasts one minute gives the slot back, or keeps it for a grace | `main`, `wait-grace` |
| `root-idle-while-children-run` | a tree root holds its slot with no turn while its children run, and under `main` waits for them in `awaiting_children` | `main`, `legion-v22.14.1` |
| `p0-at-a-full-cap` | a P0 starts at a full regular cap with no P0 bound, but waits under the reference rule | `main`, `p0-at-once`, `before-p0` |
| `p0-at-its-bound` | the first P0 fills a bound of one, and the pass refuses the second P0 in first place | `main`, `p0-at-once` |
| `urgent-work-waits` | the first `todo`, a priority raise and the run-out determine the waits of P0 and P1 work | `main`, `p0-at-once` |
| `conflict-in-accepting` | a conflict in accepting returns work to implementing | `legion-v20.42.5` |
| `conflict-in-retro` | a conflict in retro returns work to implementing | `legion-v20.42.5` |
| `signal-crosses-with-no-event` | the tick still starts P0 and returns a slot, but only the signal deadline starts lower-priority work | `main`, `signals` |
| `no-crossing-due` | no deadline is due at first, and a new kill moves the pending deadline before it fires | `signals` |
| `p0-inherited-across-projects` | at a full cap, a child that inherits P0 from another project's ancestor starts, an explicit P1 child waits, and an ancestor's raise moves a waiting issue to P0 at that moment | `main`, `p0-at-once` |
| `p0-inherited-ancestor-missing` | the same window without the required ancestor's record: each rule with the new P0 start is refused, naming it | `before-p0`; `main` and `p0-at-once` refused |
| `p0-inherited-from-the-nearest-ancestor` | a child with no priority of its own takes its parent's P0, not the P2 of the top issue of its chain, and starts at a full cap | `main`, `p0-at-once` |
| `p0-recorded-descendant-update` | Dispatch's recorded update of a child whose own priority stays null and whose resolved priority becomes P0 starts it at a full cap | `main`, `p0-at-once` |
| `one-free-slot-opposite-order` | one free slot and two issues below P0 in opposite order under the recorded rule and by resolved priority | `main`, `p0-at-once` |
| `git-store-verdicts` | on the branch that asks git, a read that the store answers with a conflict where the record shows none, and a read that it answers clean where the record shows a conflict | `legion-v20.42.5`, `main` |
| `git-unanswered-reads` | on the branch that asks git, one read of each kind that git cannot answer, which the generator answers | `main` |
| `merge-event-time` | a merge lands in the replay's base at GitHub's merge event, not at its committer's time | `main` |
| `drawn-skipped-loop` | on the branch that draws every conflict, a stretch that goes on past a skipped conflict loop draws no second time | `main` |
| `drawn-boot-earlier-phase` | a boot's draw in an earlier phase than the one its stretch ends in enters its conflict at the stretch's end, in that phase | `legion-v20.42.5` |
| `drawn-touching-merges` | a draw counts the merges that change a path of the issue's profile, and a stretch with none draws no conflict | `legion-v20.42.5` |
| `drawn-work-conflict` | drawn work keeps its source's recorded conflict on the main branch, and draws it by its source's profile on the branch that draws | `main`, on two branches |
| `random-experiment` | a workflow experiment in the `random` mode is drawn for each issue the daemon records, keyed to no issue: the run names those draws as unkeyed, and a comparison pairs no run of the window | `main` |

The three windows of R10, the start of a P0 issue outside the cap, run it under `p0-at-once` and under `main`, which starts a P0 issue outside the cap since cybermonklabs/legion#5488.
Their expected values under `main` are those of the run under `p0-at-once`, the two rules differing only in name.

The five windows of R10 that rest on the priority Dispatch resolves for an issue, spec v36's four and the nearest-ancestor row of spec v49 (`p0-inherited-from-the-nearest-ancestor`), read it as Dispatch resolves it: an issue's own, else that of the nearest issue above it on its Dispatch parent chain that has one, in any project, up to 32 levels.
`main`, since cybermonklabs/legion#5488, and `p0-at-once` read the resolved priority for the P0 start and the line's order; the recorded rule these windows ran, `main` before that change and `before-p0` now, starts an issue outside the cap on no priority and orders its line by the issue's own priority, else its tree root's; and the measures read the resolved priority under every rule, so `urgentWaits` and `priorityTakenBanded` count an issue at its resolved priority under every rule.
Their record names an issue's parent in another project, ACMEAUX, as its events do; `p0-recorded-descendant-update` is Dispatch's record from a release that serves `resolved_priority`, and the four others are from before it, so the replay reconstructs the resolved priority from the window's parent chains.

The seven windows from `git-store-verdicts` on are worked by the spec's R2 as "The branch that asks git, and the generator's profile" below gives it.
`drawn-skipped-loop` and `drawn-boot-earlier-phase` hold no git input and exercise the generator's legacy merge count.

## What a directory holds

- `window.json`, `log.jsonl` and `events/<KEY>.jsonl` (and `merges/<owner>__<name>.jsonl` in `clock-far-from-now`, `conflict-from-fitted-rate` and the seven windows of the git branch and the generator, which hold base-branch commits, and `git/<owner>__<name>.jsonl` in the five of them that hold git's reading), in the format of `window.go`: `ReadWindow` checks each input against the sha256 `files` records.
  A window that holds a record of R11's signals holds its inputs too, each optional: `launcher/log.jsonl` (`{time, pod, text}`, the launcher's lines, in `signal-crosses-with-no-event` and `no-crossing-due`), `signals/provider.jsonl`, `queue/<owner>__<name>.jsonl` and `settings/events.jsonl`.
  Its `window.json` names each signal's spans in `signals` (`{"kills": [{"from", "to"}]}`), and may give `defaultAssignee` and the P0 bound's changes in `p0Bounds` (`[{"at", "bound"}]`, in `p0-at-its-bound`).
  It may name in `missingAncestors` each ancestor on a Dispatch parent chain whose events `collect` could not read (`["ACMEAUX-1"]`, in `p0-recorded-descendant-update`).
  Every other window names none: each holds the events of every parent its events name, but `p0-inherited-ancestor-missing`, which lacks ACMEAUX-1's as a window collected before `collect` read each ancestor can.
  `replay.md`'s "The records of the signals" gives each file's form.
  No fixture window holds `signals/memory.jsonl`, the pods' memory series that `collect` wrote before no model read it, which `ReadWindow` checks by its sha256 alone.
  After an edit of an input, write its new hash into `files`: `sha256sum log.jsonl events/*.jsonl`, adding `merges/*.jsonl`, `git/*.jsonl` and each signal input where the window has them.
  `TestFixtureWindows` reads every window with `ReadWindow` before it replays it (it needs `LEGION_TEST_PG_DSN`), and `TestTheFixtureWindowsCoverK1` reads each window of k1's list without a database, so a hash that differs fails them.

- `README.md`: the window's issues, what happens, what changes under each rule it names, and every
  expected value worked out by hand from the window's files.

- `expect.json`, with these keys:

  - `rules`: `{"<rule>": <Scorecard>}`, one `Scorecard` (`scorecard.go`, its JSON names) for each rule the window is replayed under.
    The run takes the seed and the slowdown factor of its own `Scorecard` (`seed`, `slowdown`), so `forty-two-more-in-flight` runs at f = 1.5.

  - `branch`: the branch the rules run on, named as "The branches" of `docs/site/src/content/docs/legion/replay.md` names it; `main` where the key is left out.

  - `scenarios` (`never-starts` alone, in place of `rules`): `{"<scenario>": {"<rule>":
    <Scorecard>}}`, one set for each value of `NeverStarted`; the scenario `none` runs on `main`,
    the others on `main+neverStarted=<scenario>`.

  - `branches` (`drawn-work-conflict` alone, in place of `rules`): `{"<branch>": {"<rule>": <Scorecard>}}`, one set for each branch the window runs on, each named as `branch` names one.
    A branch that changes more than one knob names each change in turn, as `main+conflicts=drawn+neverStarted=drawn`.

  - `moves` (optional): `{"<KEY>": [{"from", "to", "trigger"}, …]}`, each issue's
    recorded moves in order.
    A replay under the window's recorded rule must write the same `workflow: phase changed` lines in its own log, issue by issue, which a move the replay refuses breaks.

  - `checks`: optional additional assertions by rule.
    `run` lists exact fields in `run.json`.
    A missing field fails even if its expected value is zero or an empty array.
    `log` lists structured lines, comparing each named attribute.
    These assertions cover the bound refusal, deadline history, slot pass kinds, timed signal values, the reads of the branch that asks git and the draws of a stretch's conflict.
    `run`'s `limits` and the moments of its `signalValues` are the run's inputs too: `TestFixtureWindows` replays the window under that rule with those limits (`RunConfig.Limits`) and a probe at each of those moments (`RunConfig.Probes`), and `run.json` echoes both.
    `signalValues` gives, at each moment, each signal that `limits` names, `null` where the window holds no record of it then.
    Every window names each of R11's six signals at a moment at least once (`TestEveryWindowHasEachSignalAtAMoment`), its values worked out by hand in its README, under "The signals, worked by hand".
    A window that holds no record of a signal names it unrecorded, and the rules that admit by no signal decide nothing by its limit.

  - `refused` (optional): `{"<rule>": "<text>"}`, each rule under which `legion replay run` must refuse the window, on the window's branch, with an error that holds the text, in place of a `Scorecard` in `rules` and of `checks`.
    `p0-inherited-ancestor-missing` names `main` and `p0-at-once`, each a rule with the new P0 start, whose refusal must name `ACMEAUX-1`, the ancestor whose history the window lacks.

The values of `expect.json` are those of `scorecard.json`, the snapshot timeline (`replay.md`, the files of `legion replay run`): the timeline the replay builds from the store as each admission pass leaves it, not `scorecard-line.json`, the one `BuildTrace` builds again from the replay's own log lines.
The two may differ by the reconstruction error the tool reports; these windows hold the exact one.

## How a window is written

- Every window is one day, 2001-03-05 (`clock-far-from-now` too), a few hours long, with an admission cap of 1 to 3 (43 in `forty-two-more-in-flight`), so each value is a sum of a few whole minutes.
  Most windows start at the daemon's boot, with an empty start state.
  The conflict windows in accepting and retro and the signal window with a crossing start after boot and seed the state from the earlier record.

- Issue keys are `ACME-<n>`.
  `ACMEAUX-<n>` are synthetic issues of a different project: a blocker in `signal-crosses-with-no-event`, and the Dispatch parents of roots handed to Legion in the four windows of spec v36's R10.
  A root handed to Legion is filed `todo` with the label `legion` by a person (`user:acme-person`);
  a child is filed under its parent by the role that files it (its claim token as a session) with no label, `todo` (recorded at once under a live tree) or `backlog` (not released, which Dispatch still counts as an open child).
  An issue Legion does not work has no label.
  Ranks are short strings compared as bytes (`a0` < `a1` < `b01`).

- The record follows the daemon's own code under the rule it ran.
  `window.json`'s `parts` name the recorded rule and commit, `collector` names the collector's commit, and `release` is `fixture`.
  The log holds each line kind of the code the window's facts make, with the code's msgs and attribute keys (`window_builder_test.go` writes the same shapes): the admission pass's slot lines (`admission: slot taken|released`, the in-transaction `admission gives a slot back to …` and `admission: an issue waits for the Dispatch children under it`), the engine's `workflow:` lines, and the supervisor's `supervise: launched`, `a turn started` and `a turn ended`.
  Within one instant, a pass's in-transaction lines come before its after-commit lines.
  `window.json`'s `lines` lists every msg the log holds; a kind left out is a kind the window does not hold.
  Three are left out: the per-pass `admission waits for what this issue waits on`; `workflow: linger closed the tree` where a linger ends before `to`, which no window but `every-trigger` writes; and, in `clock-far-from-now`, `workflow: issue left the workflow` and `workflow: tree lingers` at ACME-2's move to `done`.

- The daemon's own status writes are events of the session `legion-daemon:ACME`.
  These windows write each one at the instant of the move that queued it; `clock-far-from-now` writes them a second later.
  A replay leaves them out and applies the echo of its own status write at the instant of the fact that queued it, which matters where a Legion issue's `done` ends another issue's wait (`two-waiters-one-blocker`).

- Defaults: `gateSteps` empty (the registration opens the gate at once; `gate-holds-child-first-phase` gates `spec`), `linger` 2 h, `tickInterval` 1 h, `reviewRoundCap` 3, `workerSilence` and `workerBackground` 4 h, no experiment.
  The tick changes nothing in the original windows.
  `signal-crosses-with-no-event` checks admission at a tick.

## Work segments and their replayed length

- A work segment is one visit of an issue to a phase, from the visit's worker start to the recorded move that ends it.
  The worker start is the later of the phase change into the visit and the slot the issue takes for it; in a phase that keeps no slot it is the phase change.
  A recorded segment's length is its end less the worker start: `phase-goes-on-during-link-wait`'s testing visit begins at the 11:30 move and its worker starts with the slot at 12:30, so the segment runs from 12:30 to the 13:00 move, 30 minutes.

- A visit that the admission pass ends (`gate_opened`) ends its segment at the visit's last design registration (`SegmentEndRegistration` of `trace_build.go`), or, with none, at its worker's last turn's end (`SegmentEndTurn`), however much later the move comes.
  A visit the issue's leave cuts is ended by that leave (`SegmentEndLeft`), and a visit with no move out in the window stays open (`SegmentOpen`, two bullets below).
  `every-trigger`, `gate-holds-child-first-phase` and `one-minute-wait` each hold an admitted segment that ends at its registration, before the move out of admitted.

- In a replay a segment starts at the replay's own worker start and ends at `start + length ×
  f^((n − m)/42)`, n the issues in flight in the replay at its start (after the pass that started it) and m the issues in flight in the record at the moment the record started it.
  At f = 1, or wherever n = m, a segment keeps its length.
  Its turns, and a child it files, enter at their recorded offsets from its start.

- A segment the record shows unended at the window's end has no known length: in a replay it does
  not end before the window does, and a turn open in it stays open (`child-filed-after-parent-start`
  under `main`).

- A sign-off the record shows at the instant a member beneath the issue leaves the workflow, after the architect's last turn in accepting ended earlier, is a held sign-off: its segment ends at that turn's end, where the replay applies `intake.SignOff`, which the engine refuses while the member is in the workflow, and again at each `workflow: issue left the workflow` of a member beneath.
  `every-trigger` holds one.

- A merge the record shows in `workflow: a merged issue's close waits for the issues beneath it`, its move to `done` coming only at the last such issue's leave, is a held merge: the merger's segment ends at the first such line of the visit, where the replay applies `intake.PullRequestMerged`.
  The engine leaves the issue in merging while an issue beneath it is in the workflow, which the replay counts (`heldMerges`) and never refuses, and closes it at the last one's leave with no move of the replay's.
  No window here holds one.

## Starts and slots

- A start takes its slot in the same instant as the pass that gives it, and the worker's first turn
  begins at that instant too: a promotion, a child's first phase and a returned issue's slot are
  each one pass, and a fact's own pass gives out the slot it frees (assumption 11).

- A pass releases first and then promotes in the line's order (`record.Waiting`): the issues whose
  work came back, oldest first, then the issues whose first phase can start, by their tree root's
  rank and then their own.

- A phase under way goes on while its issue waits on a `blocked_by` link, and its worker's turns with it;
  only the next start waits, for the end of the wait and then for a slot.
  Under `main` and every rule built from it but `cover`, a started parent in `planning` through `accepting` with an open Dispatch child moves into `awaiting_children` (`record.SlotRule.WaitStopsPhase`, cybermonklabs/legion#4592): its phase stops, its worker's segment waits with the rest of its length, it holds no slot, and it comes back once no child of it is open.
  Under the recorded rules and `cover`, no wait stops the phase.

- Under `cover` (`record.coverRule`, whose choices `legion replay rules` prints, and assumption 14 of `replay.md`), a started issue keeps its slot in every phase through `held`, and no wait gives it back, for its children or a link alike.
  The line keeps `main`'s order, `returned_oldest_first`.
  A start takes `cover`: an issue that a slot holder waits on and that has not started starts at once on that holder's slot, one cover for each holder, and the holder is then a covered waiter that holds no slot until the cover ends, when the covered issue's work is done or the waiter no longer waits on it, and it takes its slot back at once.
  A second waiter on an issue already started keeps its own slot, and a waiter's next start waits for its wait to end, slot or not.

- Under `wait-grace`, a wait on another issue keeps its holder's slot for five minutes (`record.SlotRule.WaitGrace`), as "`wait-grace` and its grace" of `replay.md` gives it.

## The knobs

"The branches" of `replay.md` names each knob and its values; these windows run them so:

- `Timing` (assumption 4) places a link's removal, a child moved out, a gate's closing and the close of an outside blocker (an issue Legion does not work, of this project or another).
  Under `wall` each happens at its recorded time.
  Under `offset` each happens at the replay's start of the wait it ends plus its recorded offset from the record's start of that wait.
  The wait's start is its anchor: for a link, the later of the link's addition and the waiting issue's start;
  for a child moved out or a gate closing, the start of the parent's children wait (the later of the gate's opening and the child's filing);
  for an outside blocker's close, the start of the link wait it ends.
  An event that ends no wait in the record, or whose wait never begins in the replay, happens at its recorded time.
  A Legion blocker's own `done` is no timed event: it is the replay's own merge of that blocker.

- `Answer` (assumption 5) places a person's answer to an ask and a person's review of a spec version.
  Under `wall` it happens at its recorded time;
  under `offset` at the replay's moment of the question plus its recorded offset from the record's:
  the anchor is the ask's opening, or the registration of the version the person reviews, each itself at its recorded offset in its architect's segment.

- Every window runs `Timing` and `Answer` at `offset`.

- `AfterSendBack` runs `recorded`: each segment after a send-back keeps its recorded length from its own worker start.
  `Merges` runs `fitted`.

- `FirstStarts` runs `recorded` (assumption 36; "The branches" of `replay.md`); no window here gives it or `Merges` a choice to make.

- `NeverStarted` (assumption 8) is the work of an issue handed to Legion that the record never
  started:

  - `none`: no work.
    When the replay starts it, it takes its slot and its architect's start opens a segment with no turn and no end, so it holds the slot to the window's end with no worker in a turn, in flight and never done.

  - `drawn`: the work of an issue drawn with the run's seed from the issues the record started in
    the window: that issue's segments from its own start, each at its recorded offset from the new
    issue's start (and unended where the drawn issue's was).

  - `drawn-done`: the same, drawn from the issues the record finished in the window.
    A draw is made certain by giving every candidate the same work over the time the drawn work runs in the window (`never-starts`).
    No scenario is a bound.

## Conflicts

- A stretch is one visit of an issue to the phases from testing to merging:
  from its move into testing to the move that ends the visit (its merge, a conflict, or another move back).
  M is the number of first-parent commits of the base branch (`merges/`) strictly after the stretch's start and strictly before its end;
  a stretch is a conflict where a recorded `conflict` move ended it, where a merger's move back out of merging ended it and the daemon's post of that move's reason names a conflict, or where a merger was asked to merge the base forward inside it.
  A held merge ends its stretch at the merge's own line, not at the later move to `done`.

- The chance of a conflict is `1 / (1 + exp(−(b0 + b1 × log2(1 + M))))`, b0 and b1 fitted by maximum likelihood on the window's own recorded stretches, those that start at or after `from` and end before `to` (a stretch that began in the reach is left out); `Merges = zero` sets b1 to 0.
  A window that records no conflict fits a chance of 0 for every stretch, and no conflict is drawn in it.

- In a replay each stretch draws once, at the moment its recorded end falls by its segments, with M counted over the replayed stretch:
  a draw u, seeded by the run's seed, the issue, the phase and the recorded start of the segment that ends the stretch, and the number of the stretch's draws before it, makes a conflict where u < the chance, so two rules at one seed draw the same u for each stretch both run.
  A recorded `conflict` move, or a merger's move back that counts as one, is never injected: the draw makes it, or the next bullet keeps it.
  On the main branch no stretch draws: each recorded conflict is made as recorded, and its issue runs its own loop.

- A stretch whose recorded end is a `conflict` whose loop the record shows running to `to`, with no return to the phase the conflict left, does not draw:
  the record holds no work of the issue after the stretch but that loop, so a draw of no conflict, or of another loop, would leave the issue idle to the window's end (assumption 35).
  The stretch keeps that conflict, made through the daemon's own path as a drawn one is.
  Under `legion-v20.42.5` the issue then runs its own loop as recorded;
  under `main` the merge-forward segment lasts as that loop's implementing round, unended where the round is.
  The run counts each conflict kept (`divergence.keptOpenLoops`).

- Under `legion-v20.42.5` (`ConflictMovesBack`) a drawn conflict in testing, reviewing, accepting, retro or merging moves the issue back to implementing by `conflict`, and the loop's work is drawn with the run's seed among the window's recorded conflict loops that left the same phase and whose work the record shows ending: the segments from a recorded conflict move to the issue's return to the phase it left.

- Under `main` (`ConflictMergeForward`) a drawn conflict in testing, reviewing or merging keeps the phase: a merge-forward segment follows, its implementer in a turn in testing and reviewing and its merger in merging (cybermonklabs/legion#4979), its length drawn from the implementing rounds of those same loops (assumption 23), and then the issue goes on with the work the record shows after the loop returned to the phase the conflict left.
  The stretch is not drawn a second time.

- Under `legion-v22.14.1` (`ConflictImplementerMergesForward`) a drawn conflict keeps the phase in every phase, and the merge-forward segment is its implementer's, in merging too: otherwise as under `main`.

- `legion replay run` of `replay.md`, and its assumptions 23 and 37, give the rest: the loops a draw falls back on where the window records none from the conflict's phase, and under `main` a conflict in accepting or retro, which waits for merging.
  `conflict-from-fitted-rate`, the one window with a conflict, has one loop, which left merging and ended, so its draws need none of them.

## The branch that asks git, and the generator's profile

The spec's R2 gives the conflicts of a replay three sources: the main branch keeps the record's conflicts, `conflicts=git` asks git's verdicts, and `conflicts=drawn` draws every conflict.
The windows `git-store-verdicts`, `git-unanswered-reads`, `merge-event-time`, `drawn-touching-merges` and `drawn-work-conflict` hold `git/<owner>__<name>.jsonl` (`replay.md`, "Add git's verdicts of each repository") and are worked by the rules below.

- **The replay's base.**
  For a repository whose window holds git input, the base holds the record's first-parent commits (`merges/`), each from its `at` on, never from its `committed`.
  The merge commit of a pull request that an issue of the window links, whose work the replay runs, enters the base as the replay merges that issue (as it enters the issue's `pull_request_merged` move), and never where the replay does not merge it.

- **A read.**
  On `conflicts=git` the replay reads an issue's head at each end of a stretch (each move that ends one, the later end of a stretch that went on past a passed-over loop among them) and at each boot inside a stretch.
  The head is the one of the issue's pull request that stands at the read's recorded moment, the latest `at` at or before it: for an end, the recorded time of the move that ends the stretch.
  The touching merges at the read are the commits that the replay's base holds at the read, that the head does not hold (after its merge base on the record's first-parent chain), and whose `commit` line's paths meet the head's `paths`, in the order in which the replay's base took them.
  The record's touching merges of the head are the bases of its `verdicts`, in their order; they stop before the pull request's own merge.

- **An answered read.**
  The store answers a read where the replay's touching merges are the first n of the record's: git's verdict at the nth, and clean at n = 0.
  An answered conflict at a stretch's end is made through the daemon's own path, as a drawn conflict is: where the record ended that stretch in a conflict the issue runs its own loop, and where it did not the loop's work is drawn from the window's recorded loops.
  An answered clean read at an end that the record shows as a conflict lets the stretch go on past the loop, as a clean draw does.

- **An unanswered read.**
  Every other read is unanswered, of the first of these kinds that holds:
  `never-ran`, where the work is drawn or the issue has no pull request in the record;
  `missing`, where the window holds no git input for the repository or the pull request, no head stands at the read's recorded moment, a `missing` line names the head, or a commit that may touch has no `commit` line;
  `after-merge`, where the replay's touching merges hold a commit that the record's base took after the pull request's own merge (the `pull` line's `merge`);
  and `order`, every other one.
  An unanswered read goes to the generator, and a live stretch draws once.

- **After a loop the record does not hold.**
  A loop whose work is drawn, one that an answered conflict began at a stretch the record ended otherwise or one that the generator drew, leaves the issue's head merged with the replay's base at the loop's end: the return to the phase the conflict left, or the landing of the merge forward.
  A later read of that issue counts its touching merges only among the commits that the replay's base took after that loop's end: with none the read is answered clean, n = 0, and with one or more it is unanswered, `order`, since the replay merged those changes in an order the record does not hold.
  The generator counts a later stretch's M the same way.
  A loop that is the record's own needs no such rule: the recorded heads after it carry the record's merge forward.

- **What a run writes.**
  Each read writes one line of the replay's own log, `replay: a head's merge state read from git`, with `issue`, `repo`, `head`, `recorded` (the read's recorded moment), `by` (`end` or `boot`), `n` and `answer` (`clean` or `conflict`), or, for an unanswered read, `answer` `unanswered` and its `kind`.
  `run.json` counts the reads of each repository in `gitReads` (`clean`, `conflict`, `order`, `after-merge`, `missing` and `never-ran`), and gives in `gitLoopHours` the hours of the conflict loops that an answered read began (`answered`) and that the generator began (`drawn`).

- **The generator.**
  On `conflicts=drawn`, and for an unanswered read, a stretch draws as "Conflicts" above says, with M the stretch's touching merges where the repository holds git input.
  An issue's profile is the `paths` of its head that stands at the draw, and the touching merges are the commits of the replay's base at the draw that change a path of the profile and came after that head's merge base.
  A stretch with no touching merge draws no conflict: its chance is 0, and its draw's line says `merges` 0 and `chance` 0.
  The model is fitted on the window's recorded stretches, with M counted the same way on the record's base before each stretch's end; a stretch with M = 0 is left out of the fit.
  Work that the record never ran has no head: it takes the profile of the issue that its scenario drew its work from, and its touching merges are those that the replay's base took after the later of the replay's start of its first implementing segment and the landing of its last merge forward.
  On the main branch such work keeps the recorded conflicts of the issue it was drawn from.
  A repository with no git input keeps the count of "Conflicts" above, every commit strictly inside the stretch (`drawn-skipped-loop`, `drawn-boot-earlier-phase`).

## The measures

"The measures" of `replay.md` defines each measure, and the windows read each one so, with these rules the page leaves out.

- Every measure is read over [from, to), and intervals are half-open, so an issue that leaves at the instant another enters is not counted with it.

- `source` is `replay`.
  `firstDayApart` is left out: every window is shorter than a day.
  `unplaced` is 0.

- Under `main` and `legion-v22.14.1` a drawn conflict that keeps its phase is one round of `conflict` in `loops`, its hours the merge-forward segment.

- In `waits`, a wait goes on while the root of its tree lingers, and ends where its children close or move out, or its blockers are done.
  A wait that begins at the moment another ends, a link done as a child is filed, is the same stretch and counts once, ended by what ended the last.

- In `slotHoursInTurn`, a worker's turn runs from `supervise: a turn started` to `a turn ended`, its claim's failure, or its issue's leave.

- In `medianSlotWaitMinutes`, a queued root waits for a slot from its filing (or the end of its links), a child from the moment its first phase can start, and a returned issue from its owed start or the end of its wait.
  An issue that takes its slot in the pass where it begins to wait waited 0; an even count takes the mean of the two middle values.

- `priorityTakenBanded` is `priorityTaken` in today's line's order, the band of the priority Dispatch resolves for each issue first (its own, else that of the nearest issue above it that has one; P0 first, none after P3), under every rule.
  In a window where no issue has a priority every issue stands in one band, so the two are equal; `expect.json` gives `priorityTakenBanded` wherever it is not 0.
  `urgentWaits` counts each issue by the same resolved priority, under every rule.
  A window that lacks the history of an ancestor such a priority rests on gives `priorityTakenBanded` 0 and no `urgentWaits` (`p0-inherited-ancestor-missing`).

- `urgentWaits` holds `P0` and `P1` where an issue counts in them, and is left out of `expect.json` where none does.
  Its waits run past `to` to the end of the run-out, and are cut there (`cut`) for an issue with no first slot or no `done` by then; an issue that stood `todo` from before `from` counts from `from`.

- `assumptions`: the numbers of the assumptions (`assumptions.go`) the run's measures rest on, each named where the run holds the fact it is about: 1 always; 2 where an issue stands past its review; 3 where a send-back loop runs or a conflict is drawn; 4 where an issue waits on another issue or a gate closes; 5 where a person's answer or review moves the run; 6 where the start state places an issue; 7 where a hold other than the cap (a design gate, a link on a child, an ancestor, a tree) holds the first start of an issue not yet started (a started issue's wait on another issue is 4); 8 where the window holds an issue handed to Legion that the record never started; 9 where the run gives out a slot, which the line's order chooses; 10 where the window holds a link or a child, a root's Dispatch parent of an issue Legion does not work included; 11 where a pass gives out a slot it freed; 13 where a loop moves an issue back; 14 where a cover is taken; 15 where work is left at `to`; 16 where an issue reopens; 17 where a child is filed after its parent started; 18 where the window records a conflict; 23 where a conflict is drawn under `main`; 24 where the daemon is down between two boots; 25 where a blocker is in another project.
  `clock-far-from-now` names 1, 2, 4, 9, 10, 11 and 15 by these.
