# The window `conflict-in-accepting`

The daemon that runs `legion-v20.42.5` boots at 09:00 UTC. The measured span is 10:00–12:00. ACME-1 takes its slot at 09:00 and reaches reviewing at 10:00 after three 15-minute segments and a 15-minute test. At 10:15 the review approves, the issue enters accepting and the pass returns the slot.

The issue stays in accepting until the conflict. At 11:00 a conflicting head in `accepting` sends it back to implementing by `conflict`. It takes the free slot in that pass and stays in its implementer turn through 12:00. The recorded loop is still open at the end, so the replay keeps this conflict without a draw.

ACME-1 is in flight for 120 minutes, with a maximum and mean of one. It holds a slot in a turn for 15 + 60 = 75 minutes and stands past the review without a slot for 45 minutes. The open conflict loop counts once, for 60 minutes. The one grant in the span waits zero minutes. No issue finishes.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 10:00, the window's start, the run under `legion-v20.42.5` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all six signals (`limits`), and run.json echoes them; the rule admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `refusals`: no minute of the provider's record before 10:00.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
