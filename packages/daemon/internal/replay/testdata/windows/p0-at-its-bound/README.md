# The window `p0-at-its-bound`

Cap 1 is full from 09:05 UTC, with ACME-1 holding the regular slot. The P0 bound becomes 1 at 09:07. ACME-2, the first P0, takes its slot at 09:10. Both remain in their first turns through 10:00.

ACME-4 waits below P0 from 09:20. ACME-3 (P0) takes first place at 09:30 despite its worse board rank. The pass refuses ACME-3 at bound 1 and releases neither holder. It logs `admission holds a P0 issue at the P0 bound` with the issue (ACME-3), the P0 holders (1, ACME-2), the bound (1), the other holders (1, ACME-1) and the cap (1), the line cybermonklabs/legion#5488 writes, and the record holds that line at 09:30.

R10 is the start of a P0 issue outside the cap under the P0 bound.
`main` runs it, since cybermonklabs/legion#5488.
So the values below are those of `main`; the window's change of the bound at 09:07 makes a pass of its own, which starts nothing, since no P0 issue waits then.
`expect.json` held them under `p0-at-once` too, the candidate that ran R10 before that change, until the repository kept that rule no more: since that change it was `main` in every choice but its name.

Both waiters outrank ACME-1 and rank behind ACME-2, with or without the priority band. The priority-taken reading is therefore the same even though the two orders disagree on which waiter comes first.

The two grants have zero wait. In-flight and slot time in turns are each 55 + 50 = 105 minutes, with a maximum of two issues in flight and a mean of 105/60. From 09:20 to 09:30 one of two holder/waiter pairs is inverted. From 09:30 to 10:00 two of four are inverted. Priority taken is 20/60. There are no returns, completions or loops.

As said above, both orders give the same pairs, so `priorityTakenBanded` is also 20/60. The wait of urgent work holds the two P0 issues, its waits cut at 10:00 (the window has no run-out). ACME-2 is `todo` at 09:10 and takes its slot then, a wait of 0, and is not done: 50 minutes, cut. ACME-3 is `todo` at 09:30 and has neither: 30 and 30 minutes, each cut. The first-slot waits 0 and 30 give a median of 15 and a 90th percentile of 30, one cut; the waits to `done`, 30 and 50, a median of 40 and a 90th percentile of 50, two cut.

The run names assumptions 1 (always), 8 (ACME-3 and ACME-4 are handed to Legion and the record never starts either), 9 (the run gives out slots) and 15 (work is left in flight at 10:00).

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under each rule that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
The run limits all six signals (`limits`), and run.json echoes them; `main` admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `refusals`: no minute of the provider's record before 09:00.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
