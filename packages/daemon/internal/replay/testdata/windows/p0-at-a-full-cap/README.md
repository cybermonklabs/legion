# The window `p0-at-a-full-cap`

Project ACME boots at 09:00 UTC with cap 1. No P0 bound is configured.

- ACME-1 takes the regular slot at 09:05. Its first turn is still open at 10:00.
- ACME-2, P0, enters `todo` at 09:20. The same pass starts it under `main`, outside the regular cap.
- Under `before-p0`, ACME-2 waits for the whole 40 minutes after filing. ACME-1 is the sole holder and ranks behind ACME-2 throughout this wait.

Under `main`, both issues remain in flight. Their 55 + 40 = 95 minutes in flight are all slot time in turns. The maximum is 2 and the mean is 95/60. Under `before-p0` only ACME-1 starts: 55 minutes in flight and in a turn, maximum 1, mean 55/60. Priority taken is 40/60 under `before-p0` and zero under `main`. Every slot granted has zero wait. No issue finishes, returns a slot, or enters a loop.

ACME-2 (`a00`, P0) ranks above ACME-1 (`a01`, no priority) by rank alone and in today's line's order, where P0 comes before an issue with none, so `priorityTakenBanded` equals `priorityTaken`: 40/60 under `before-p0` and zero under `main`. ACME-1 has no priority, so the wait of urgent work holds ACME-2 alone, P0 from its first `todo` at 09:20, its waits cut at 10:00 (the window has no run-out). Under `main` it takes its slot at 09:20, a wait of 0, and is not done: 40 minutes, one cut. Under `before-p0` it has neither: 40 and 40 minutes, each cut.
