# The window `urgent-work-waits`

The measured span is 09:00–10:00 UTC. Replay continues to 10:15. Cap 1 is never reached by work below P0 that holds a slot in this record: ACME-1 and ACME-2 take exempt slots. ACME-3's link to an unfinished issue keeps it from starting, even though there is room.

R10 is the start of a P0 issue outside the cap.
`p0-at-once` runs it now, and `main` runs it once cybermonklabs/legion#5488 merges; until then `main` starts no P0 issue outside the cap, and its run waits on the skip list (`pending_test.go`, ruling D16).
So the values below are those of `p0-at-once` now, and of `main` after that merge.

| Issue | First `todo` | Priority before first slot | First slot | `done` |
| --- | --- | --- | --- | --- |
| ACME-1 | 09:05 | P0 throughout | 09:05 | Not by 10:15 |
| ACME-2 | 09:10 | P1, raised to P0 at 09:30 | 09:30 | Not by 10:15 |
| ACME-3 | 09:20 | P1 throughout | Not by 10:15 | Not by 10:15 |

ACME-2 also waits on the unfinished blocker until the raise. Its first `todo` remains 09:10, so its urgent first-slot wait is 20 minutes, unlike its zero time on the eligible line. P0 first-slot waits are 0 and 20 (median 10, p90 20, no cuts). P0 waits to `done` are 70 and 65 (median 67.5, p90 70, two cuts). The single P1 wait is 55 minutes for each endpoint, with one cut in each. The cuts use 10:15, not 10:00.

The two turns remain open. They account for 55 + 30 = 85 minutes of the measured span in flight and in slots with a turn. Maximum in flight is 2. There are two grants and no completions or loops.
