# The window `p0-at-a-full-cap`

Project ACME boots at 09:00 UTC with cap 1. No P0 bound is configured.

- ACME-1 takes the regular slot at 09:05. Its first turn is still open at 10:00.
- ACME-2, P0, enters `todo` at 09:20. The same pass starts it under `main`, outside the regular cap.
- Under `before-p0`, ACME-2 waits for the whole 40 minutes after filing. ACME-1 is the sole holder and ranks behind ACME-2 throughout this wait.

Under `main`, both issues remain in flight. Their 55 + 40 = 95 minutes in flight are all slot time in turns. The maximum is 2 and the mean is 95/60. Under `before-p0` only ACME-1 starts: 55 minutes in flight and in a turn, maximum 1, mean 55/60. Priority taken is 40/60 under `before-p0` and zero under `main`. Every slot granted has zero wait. No issue finishes, returns a slot, or enters a loop.
