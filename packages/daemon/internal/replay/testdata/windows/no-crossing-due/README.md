# The window `no-crossing-due`

The daemon boots at 09:00 UTC. The cap and the ceiling are both one. The only limit is `kills = 2`, with starts permitted below two. There are no issues or slot moves, so all occupancy, waits, completions and slot-hours are zero.

The first kill at 09:40 leaves the count at one. No threshold crossing is due, including at the ticks before then. The second kill, at 09:50, sets a deadline for 10:40. A third kill at 10:35 moves that deadline to 10:50: the count at 10:40 would only fall from three to two. At 10:50 the count drops to one. The deadline fires once and leaves nothing pending. The last kill ages out at 11:35 without another crossing.

The launcher series is the controlled signal input to the scheduler in this fixture. The expected deadline list is exact, so an early timer, a periodic pass, or a fire at the displaced 10:40 time fails the check.

## The signals, worked by hand

The window holds a record of the kills alone, from 09:00 to 12:00, with the launcher's lines of the kills of the pod of ACME-1, an issue the window does not hold, at 09:40, 09:50 and 10:35.
No segment holds them, so the kill model counts them apart and keeps each at its recorded time.

Under `signals`, with its limit of 2 kills alone, the run writes the kills at 09:41, 10:40 and 10:50, as the paragraph above works them out: 1, 2 and 1.

The window also runs under `main`, which gives the same scorecard but for its rule's name: no issue, so every measure is zero, and assumption 1 alone.
The run limits all six signals, which `main` admits by none, so it keeps no deadline (`deadlines` is `[]`) and moves no slot (`slotPasses` is `[]`), under the ceiling of the window's one cap, 1.
At each of 09:41, 10:40 and 10:50 it writes:

- `idle`, `queue`, `provider`, `refusals` and `approvals`: unrecorded, since the window names no span of any of them and holds no provider's record, merge queue file or settings events.
- `kills`: 1 at 09:41 (09:40's kill), 2 at 10:40 (09:50's and 10:35's, 09:40's having left the hour) and 1 at 10:50 (10:35's alone), the kill model's moments being the record's under every rule.
