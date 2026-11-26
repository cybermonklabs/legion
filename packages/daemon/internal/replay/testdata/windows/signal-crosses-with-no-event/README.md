# The window `signal-crosses-with-no-event`

The recorded daemon boots at 09:00 UTC under `legion-v19.57.3` with cap three. The measured span starts at 10:20 while that process is serving. ACME-1 takes a slot at 09:10 and is implementing by 09:40. It acquires an unfinished link to ACME-4 at 09:55 but keeps its slot under the recorded rule. Both replay rules seed that same holder and wait. Neither has a boot or another pass before the 10:31 tick, which first judges the inherited slot under the new rule.

ACME-2 (P0) and ACME-3 (below P0) wait on ACMEAUX-1. The blocker belongs to another project. Its close at 10:31 updates the replay's copy of Dispatch without applying an intake fact. The tick at the same instant sees the close on its reread. The expectation checks the pass kind as well as the timestamp. The launcher records kills at 09:40 and 09:50. The `signals` run uses a ceiling of three and a `kills` limit of two: a count of two blocks admission below P0.

| Time | `main` | `signals` |
| --- | --- | --- |
| 10:31 tick | Gives back ACME-1's slot. Starts ACME-2 and ACME-3. | Gives back the same slot and starts ACME-2, not ACME-3. |
| 10:40, no event | No pass | The older kill ages out. The count drops from two to one. The deadline pass starts ACME-3. |
| 10:50 event | Both starts are already in the record. | ACME-3 already holds its slot. |

Both runs start with one issue in flight. ACME-1 works throughout all 35 minutes, with a slot for the first 11. ACME-2 holds a slot in a turn for the last 24 minutes. ACME-3 contributes 24 minutes under `main` and 15 under `signals`. The totals for slot time in turns are 59 and 50 minutes. In-flight time is 83 and 74 minutes, with a maximum of three. ACME-1 waits on its open link throughout the measured span. It is started without a slot for the last 24 minutes. There are two grants in each run, neither to the seeded holder. Both grants wait zero minutes under `main`. Under `signals`, ACME-2 waits zero and ACME-3 waits nine, so the median is 4.5 minutes.

The deadline expectation uses the hand-worked kill series from the launcher for this fixture, not a stochastic fit. The model's fitting and draws are tested separately. `checks` distinguishes an event, tick and deadline even when they happen to produce the same final scorecard.

ACME-2 is the one issue with a priority: P0, filed `todo` at 10:10, before the measured span, and still `todo` at its start, so its wait of urgent work counts from 10:20. Under both rules it takes its slot at 10:31, a wait of 11 minutes, and is not done by 10:55, the end of the window, which has no run-out: 35 minutes, one cut. No pair of a holder and a waiting issue has the holder below in either order, since ACME-2, P0 and `a02`, holds above ACME-3, with no priority and `a03`, so `priorityTakenBanded` is 0 as `priorityTaken` is.

## The signals, worked by hand

The window holds a record of the kills alone, from 09:00 to 10:55 (`signals.kills` in `window.json`), with the launcher's lines of the kills of ACME-1's pod at 09:40 and 09:50.
Both kills fall before the window's start, where the replay keeps the record's course, so the kill model keeps each at its recorded time.
The window holds no span of any other signal, and no provider's record, no merge queue file and no settings events.

Under `signals`, with its limit of 2 kills alone, the run writes the kills at 10:31 and 10:40, as the table above works them out: 2 at 10:31, the hour from 09:31 holding both kills, and 1 at 10:40, once 09:40's leaves the hour.

Under `main`, the run limits all six signals, which `main` admits by none, so they decide nothing and `main` keeps no deadline (`deadlines` is `[]`).
At 10:31 it writes:

- `idle`: unrecorded, since the window names no span of the idle share's record.
- `queue`: unrecorded: no merge queue file and no span of its record.
- `provider`: unrecorded: no minute of the provider's record.
- `refusals`: unrecorded: no minute of the provider's record before 10:31.
- `kills`: 2, the kills of 09:40 and 09:50, both in the hour from 09:31, inside the record's span.
- `approvals`: unrecorded, since the window names no span of the approvals' record.
