# The window `conflict-in-accepting`

The daemon that runs `legion-v20.42.5` boots at 09:00 UTC. The measured span is 10:00–12:00. ACME-1 takes its slot at 09:00 and reaches reviewing at 10:00 after three 15-minute segments and a 15-minute test. At 10:15 the review approves, the issue enters accepting and the pass returns the slot.

The issue stays in accepting until the conflict. At 11:00 a conflicting head in `accepting` sends it back to implementing by `conflict`. It takes the free slot in that pass and stays in its implementer turn through 12:00. The recorded loop is still open at the end, so the replay keeps this conflict without a draw.

ACME-1 is in flight for 120 minutes, with a maximum and mean of one. It holds a slot in a turn for 15 + 60 = 75 minutes and stands past the review without a slot for 45 minutes. The open conflict loop counts once, for 60 minutes. The one grant in the span waits zero minutes. No issue finishes.
