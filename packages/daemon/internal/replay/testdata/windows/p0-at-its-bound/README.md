# The window `p0-at-its-bound`

Cap 1 is full from 09:05 UTC, with ACME-1 holding the regular slot. The P0 bound becomes 1 at 09:07. ACME-2, the first P0, takes its slot at 09:10. Both remain in their first turns through 10:00.

ACME-4 waits below P0 from 09:20. ACME-3 (P0) takes first place at 09:30 despite its worse board rank. The pass refuses ACME-3 at bound 1, logs the issue, first place and bound, and releases neither holder.

Both waiters outrank ACME-1 and rank behind ACME-2, with or without the priority band. The priority-taken reading is therefore the same even though the two orders disagree on which waiter comes first.

The two grants have zero wait. In-flight and slot time in turns are each 55 + 50 = 105 minutes, with a maximum of two issues in flight and a mean of 105/60. From 09:20 to 09:30 one of two holder/waiter pairs is inverted. From 09:30 to 10:00 two of four are inverted. Priority taken is 20/60. There are no returns, completions or loops.
