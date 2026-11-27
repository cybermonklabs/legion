# The window `p0-inherited-ancestor-missing`

The window `p0-inherited-across-projects` with the record of one required ancestor removed: every file is that window's but `events/ACMEAUX-1.jsonl`, which this window does not hold, and `window.json`'s `files`, which no longer names it.
It holds check k1's second copy of the full-cap window, with the required Dispatch ancestor record removed (spec R10: "If the record lacks a required ancestor's history, report the missing input and make no comparison that depends on it").
`window.json` names no `missingAncestors`, as a window collected before `collect` read each ancestor names none: the record lacks ACMEAUX-1's history, and the window's own events say so, since ACME-2's and ACME-3's name it as their parent.

## Which ancestor is required

The issues, their times and the record are those of `p0-inherited-across-projects` (its README's tables).
ACME-2 has no priority of its own, and its Dispatch parent is ACMEAUX-1, so its resolved priority rests on ACMEAUX-1's history, which the window does not hold: ACMEAUX-1 is a required ancestor.
ACME-3's parent is ACMEAUX-1 too, but ACME-3 has a priority of its own, P1, which Dispatch resolves to with no read of any ancestor, so its priority rests on no ancestor.
ACME-4's chain is ACMEAUX-2's, whose history the window holds, and ACME-1 has no parent.

## The runs

**Under `main`, refused.**
A rule with R10's start of a P0 issue outside the cap decides ACME-2's start by its resolved priority, which the window cannot give, so `legion replay run` refuses the window, and its refusal names ACMEAUX-1, the missing input.
`main` is such a rule since cybermonklabs/legion#5488.
`expect.json` gives the run under `refused`, with the text `ACMEAUX-1` that the refusal must hold, and gives no scorecard of it.

**Under `before-p0`, which runs.**
`before-p0` is `main` with no P0 issue started outside the cap: in every choice it is the rule this window was recorded under, `main` at `0d3786da`, before R10 reached the daemon.
It reads no resolved priority for any start, so it runs on the window, and its admissions are those of `p0-inherited-across-projects` under the recorded rule: ACME-1 alone holds a slot, from 09:05 to 10:00, in a turn, and nothing outside the cap starts.
So `slotHoursInTurn` and `inFlight` are 55/60 = 0.91666…, at most 1 in flight, one slot, `firstSlot` ACME-1 09:05, `medianSlotWaitMinutes` 0, `workLeft` 1, priority taken by rank 0 (ACME-1 at `a00` above each waiting issue), and `assumptions` 1, 8, 9, 10 (the window holds ACME-2's, ACME-3's and ACME-4's parent links) and 15.
The two measures that band an issue by its resolved priority rest on ACME-2's: the wait of urgent work, in which ACME-2 would count in P0, and priority taken in today's line's order, in which ACME-2 would rank above ACME-1.
The run gives neither: `expect.json` leaves `urgentWaits` out and gives `priorityTakenBanded` as 0, the scorecard's value for a measure the run does not read, and the run is to name ACMEAUX-1 as the input the two lack, which `expect.json` does not hold.

## What moved with cybermonklabs/legion#5488

Since cybermonklabs/legion#5488 `main` starts a P0 issue outside the cap and reads the priority Dispatch resolves, so it is refused on this window as the candidate `p0-at-once` was: `expect.json` moves its run from `rules.main`, a scorecard, to `refused.main`, `ACMEAUX-1`.
The scorecard worked above for the recorded rule moves with that rule's choices to `before-p0`, with no value changed but the rule's name.
`refused` named `p0-at-once` too, with the same text, until the repository kept that rule no more: since that change it was `main` in every choice but its name.

## What the window shows

A replay that guesses the missing ancestor's priority, or reads ACME-2 as having none, and so compares a rule with the new P0 start on this window, fails it.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `before-p0` writes each signal unrecorded (`null`) in `signalValues`; the run under `main` is refused, and writes none.
The run limits all five signals (`limits`), and run.json echoes them; `before-p0` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
