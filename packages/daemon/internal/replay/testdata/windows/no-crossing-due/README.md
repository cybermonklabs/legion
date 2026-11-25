# The window `no-crossing-due`

The daemon boots at 09:00 UTC. The cap and the ceiling are both one. The only limit is `kills = 2`, with starts permitted below two. There are no issues or slot moves, so all occupancy, waits, completions and slot-hours are zero.

The first kill at 09:40 leaves the count at one. No threshold crossing is due, including at the ticks before then. The second kill, at 09:50, sets a deadline for 10:40. A third kill at 10:35 moves that deadline to 10:50: the count at 10:40 would only fall from three to two. At 10:50 the count drops to one. The deadline fires once and leaves nothing pending. The last kill ages out at 11:35 without another crossing.

The launcher series is the controlled signal input to the scheduler in this fixture. The expected deadline list is exact, so an early timer, a periodic pass, or a fire at the displaced 10:40 time fails the check.
