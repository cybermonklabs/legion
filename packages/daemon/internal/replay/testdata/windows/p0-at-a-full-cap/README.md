# The window `p0-at-a-full-cap`

Project ACME boots at 09:00 UTC with cap 1. No P0 bound is configured.

- ACME-1 takes the regular slot at 09:05. Its first turn is still open at 10:00.
- ACME-2, P0, enters `todo` at 09:20. The same pass starts it under R10, outside the regular cap.
- Under `before-p0`, ACME-2 waits for the whole 40 minutes after filing. ACME-1 is the sole holder and ranks behind ACME-2 throughout this wait.

R10 is the start of a P0 issue outside the cap.
`main` runs it, since cybermonklabs/legion#5488; `before-p0` is the reference without that start.
So the values below under R10 are those of `main`.
`expect.json` held them under `p0-at-once` too, the candidate that ran R10 before that change, until the repository kept that rule no more: since that change it was `main` in every choice but its name.

Under R10, both issues remain in flight. Their 55 + 40 = 95 minutes in flight are all slot time in turns. The maximum is 2 and the mean is 95/60. Under `before-p0` only ACME-1 starts: 55 minutes in flight and in a turn, maximum 1, mean 55/60. Priority taken is 40/60 under `before-p0` and zero under R10. Every slot granted has zero wait. No issue finishes, returns a slot, or enters a loop.

ACME-2 (`a00`, P0) ranks above ACME-1 (`a01`, no priority) by rank alone and in today's line's order, where P0 comes before an issue with none, so `priorityTakenBanded` equals `priorityTaken`: 40/60 under `before-p0` and zero under R10. ACME-1 has no priority, so the wait of urgent work holds ACME-2 alone, P0 from its first `todo` at 09:20, its waits cut at 10:00 (the window has no run-out). Under R10 it takes its slot at 09:20, a wait of 0, and is not done: 40 minutes, one cut. Under `before-p0` it has neither: 40 and 40 minutes, each cut.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under each rule that `expect.json` names writes each signal unrecorded (`null`) in `signalValues`.
Each run limits all six signals (`limits`), and run.json echoes them; no rule here admits by signals, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `refusals`: no minute of the provider's record before 09:00.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
