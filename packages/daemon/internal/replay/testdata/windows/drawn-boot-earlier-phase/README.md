# The window `drawn-boot-earlier-phase`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
four hours, from 2001-03-05T09:00Z to 13:00Z, an admission cap of 1, one issue, recorded under the rule `legion-v20.42.5` (a conflicting head moves back to implementing by `conflict`), with a daemon process that stops and the next that starts at 10:00.
It holds one fact of check k1: on the branch that draws every conflict, a stretch under way at a daemon boot in an earlier phase than the one it ends in draws at the boot, at the chance of the phase the record ends it in, and its conflict enters at the stretch's end in that phase, never in the earlier one (spec R2, assumption 3).
`merges/acme__widgets.jsonl` holds the base branch's two first-parent commits; the window holds no git input, so M counts every commit of the base strictly inside a stretch.
`expect.json` holds the scorecard of a replay under `legion-v20.42.5` on the branch `main+conflicts=drawn`, with the line of the boot's draw and the conflict's move, worked by hand below.
Each draw is certain at the precision "The fit" below gives, so every seed gives the same values.

## What happens (the record)

ACME-1, a root handed to Legion, rank `a0`, filed at 09:00.

| time (UTC) | fact | what the daemon does |
| --- | --- | --- |
| 09:00 | the daemon boots; ACME-1 filed, `todo`, `legion` | promotes it: slot 1 of 1, architect turn to 09:15 |
| 09:15 | design registered | planning; turn to 09:30 |
| 09:30 | `planner_completed` | implementing; turn to 09:45 |
| 09:45 | `implementation_ready` | testing (stretch 1 begins); tester turn to 10:15 |
| 09:50 | a commit of no issue of the window, `9ef6202e…`, lands on `main` | |
| 10:00 | the daemon stops (`legion daemon stopped`) and the next process boots | the boot reads each pull request again; the tester's turn goes on |
| 10:15 | `tester_passed` | reviewing; turn to 10:30 |
| 10:30 | `conflict` | reviewing to implementing, on its slot (stretch 1 ends in a conflict); implementer turn to 10:45 |
| 10:45 | `implementation_ready` | testing (stretch 2 begins); turn to 11:00 |
| 11:00 | `tester_passed` | reviewing (the conflict loop is back in the phase it left); turn to 11:15 |
| 11:15 | `review_approved` | accepting; its slot goes back; architect turn to 11:30 |
| 11:30 | `sign_off` | retro; turn to 11:45 |
| 11:45 | `retro_completed` | merging; turn to 12:00 |
| 12:00 | `pull_request_merged` | done; its merge lands `fcfccea6…` |

Every segment is 15 minutes but the first testing segment, 30 minutes from 09:45 to 10:15.
The conflict loop is implementing 10:30 to 10:45 and testing 10:45 to 11:00.

## The fit

A stretch that a boot read inside counts M to that read, where a replay draws it.

| stretch | from | to | ends in | spans the boot | commits counted | M |
| --- | --- | --- | --- | --- | --- | --- |
| first | 09:45 | 10:30 | a conflict, in reviewing | yes, at 10:00 | 09:50, strictly between 09:45 and 10:00 | 1 |
| second | 10:45 | 12:00 | its merge | no | none | 0 |

The stretches separate: the one at M = 1 conflicted and the one at M = 0 did not.
The fitted chance is 0 at M = 0 and 1 at M = 1, each to within 10⁻⁶.
No phase takes a term of its own and no boot term is fitted: each needs a phase's stretches to hold both a conflict and a stretch without one.

## The replay under `legion-v20.42.5`, on `main+conflicts=drawn`

The recorded rule, so each segment starts at its recorded time.

- 10:00, the boot: ACME-1's stretch has run from 09:45 and its tester's segment runs to 10:15, so the boot draws the stretch, once.
  The record ends this stretch in reviewing, so the draw reads reviewing's chance and takes the place of the reviewing segment that ends the stretch, while the piece that runs is the testing one.
  M counts the commits strictly between 09:45 and 10:00, the one of 09:50: M = 1, a chance of 1, a conflict.
  ACME-1 stands in testing, not in reviewing, so the conflict waits: the draw's line says `running` `testing`, `fitted` `reviewing`, `by` `boot`, `merges` 1 and `outcome` `deferred`, and nothing moves at 10:00.

- 10:15: `tester_passed` as recorded, reviewing.

- 10:30: the reviewing segment ends the stretch in the fitted phase, and the conflict that waited enters, with no draw of its own: `conflict` moves ACME-1 from reviewing to implementing.
  The loop's work is drawn from the window's recorded loops, of which there is one, ACME-1's own, so it runs as recorded: implementing to 10:45, testing to 11:00, back in reviewing at 11:00.

- 12:00: the second stretch, from 10:45, draws at its merge with M = 0: clean, and ACME-1 merges at 12:00.

The replay is the record.
A tool that let the boot's conflict act at 10:00 would move ACME-1 back from testing at 10:00, cut its tester's segment there and give another conflict loop and log.

## The scorecard under `legion-v20.42.5`, worked by hand

The span is 4 hours.

**Slots.**
ACME-1 from 09:00 to 11:15, 135 minutes, through the conflict's loop (reviewing and implementing both keep the slot): one slot on 2001-03-05, wait 0, median 0; `firstSlot` ACME-1 09:00.

**Slot-hours.**
A turn runs from 09:00 to 11:15 with no gap: `slotHoursInTurn` 2.25 hours, `slotHoursNoTurn` 0.

**In flight.**
09:00 to 12:00, 3 hours, an average of 0.75, at most 1.

**Done and work left.**
Done at 12:00 (`done` 1); `workLeft` 0.

**Started work without a slot.**
ACME-1 holds its slot when the conflict moves it back: 0 in both readings.

**Past the review.**
11:15 to 12:00 with no slot: `pastReviewNotHolding` 45 minutes, 0.75 hours, an average of 0.1875, at most 1; `pastReviewHolding` 0.

**Priority taken.**
No issue waits for a slot: 0.

**Loops.**
One round of `conflict`, from the move back at 10:30 to ACME-1's return to reviewing at 11:00: 0.5 hours.

**Waits.**
None.

**Assumptions.**
1; 2 (past the review); 3 (a conflict and its loop); 9; 13 (the loop moves ACME-1 back); 18 (the window records a conflict).
The daemon stops and starts in one instant, so no pass falls in a downtime: not 24.
`assumptions` is [1, 2, 3, 9, 13, 18]; `scenario` is `none`.
