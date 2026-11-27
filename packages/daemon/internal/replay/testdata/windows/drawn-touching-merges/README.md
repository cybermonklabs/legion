# The window `drawn-touching-merges`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
four hours, from 2001-03-05T09:00Z to 13:00Z, an admission cap of 2, two issues, recorded under the rule `legion-v20.42.5` (a conflicting head moves back to implementing by `conflict`).
It holds one fact of check k1: on the branch that draws every conflict, a stretch whose base takes a merge that changes a path of its issue's profile draws a conflict, and a stretch whose base takes only merges that change none draws none (spec R2).
The record is the one of `conflict-from-fitted-rate`, with each issue's pull request, one more commit on `main`, and git's reading of them in `git/acme__widgets.jsonl`.
`expect.json` holds the scorecard of a replay under `legion-v20.42.5` on the branch `main+conflicts=drawn`, with the line of each stretch's draw, worked by hand below.
Each chance is exactly 0 or 1 ("The fit" below), so every seed gives the same values.

## The issues, the base and git's reading

| issue | pull request | what it is | rank |
| --- | --- | --- | --- |
| ACME-1 | acme/widgets#11 | a root whose head conflicts with its base once ACME-2 merges | `a0` |
| ACME-2 | acme/widgets#12 | a root that merges first, at 10:45 | `a1` |

| commit of `main` | pull request | `at` | paths |
| --- | --- | --- | --- |
| `5306e336…` | #30, of no issue of the window | 10:10 | `billing/invoice.go` |
| `24a7d942…` | #12, ACME-2's own | 10:45 | `aisle/sort.go`, `shelf/stock.go` |
| `6eac36fb…` | #11, ACME-1's own | 12:45 | `shelf/stock.go` |

| head | stands from | merge base | paths (the profile) | git's verdicts |
| --- | --- | --- | --- | --- |
| #11 `3b373d9e…` | 09:45 | a commit before the window | `shelf/stock.go` | a conflict at #12's commit |
| #11 `eeca3953…` | 11:30 | #12's commit | `shelf/stock.go` | none |
| #12 `805b5ecb…` | 09:50 | a commit before the window | `aisle/sort.go`, `shelf/stock.go` | none: #30's commit changes neither path |

## What happens (the record, under `legion-v20.42.5`)

| time (UTC) | ACME-1 | ACME-2 |
| --- | --- | --- |
| 09:00 | filed, `todo`, `legion`; promoted, slot 1 of 2; architect turn to 09:15 | |
| 09:05 | | filed and promoted, slot 2 of 2; architect turn to 09:20 |
| 09:15 / 09:20 | design registered: planning, turn to 09:30 | design registered: planning, turn to 09:35 |
| 09:30 / 09:35 | implementing, turn to 09:45 | implementing, turn to 09:50 |
| 09:45 / 09:50 | testing (stretch 1 begins), turn to 10:00 | testing (its stretch begins), turn to 10:05 |
| 10:00 / 10:05 | reviewing, turn to 10:15 | reviewing, turn to 10:20 |
| 10:10 | #30 lands on `main` | |
| 10:15 / 10:20 | accepting, slot back; architect turn to 10:30 | accepting, slot back; architect turn to 10:30 |
| 10:30 | retro, turn to 10:45 | retro, turn to 10:40 |
| 10:40 | | merging, merger turn to 10:45 |
| 10:45 | merging, merger turn to 11:00 | `pull_request_merged`: done; its merge lands `24a7d942…` |
| 11:00 | `conflict`: merging to implementing (stretch 1 ends in a conflict); the implementer's start takes a free slot in the same pass, turn to 11:30 | |
| 11:30 | testing (stretch 2 begins), turn to 11:45 | |
| 11:45 | reviewing, turn to 12:00 | |
| 12:00 | accepting, slot back; architect turn to 12:15 | |
| 12:15 | retro, turn to 12:30 | |
| 12:30 | merging (the conflict loop is back in the phase it left), merger turn to 12:45 | |
| 12:45 | `pull_request_merged`: done; its merge lands `6eac36fb…` | |

ACME-1's segments are 15 minutes each but the conflict round's implementing, 11:00 to 11:30, 30 minutes.
ACME-2's are 15 minutes each from 09:05 to 10:20, then accepting 10, retro 10 and merging 5 minutes.

## The fit

The window holds git input for acme/widgets, so M counts a stretch's touching merges: the commits that the base took before the stretch's end, after the merge base of the head that stands at that end, and that change a path of that head (`../README.md`, "The branch that asks git, and the generator's profile").
An issue's own merge lands at its stretch's end and is not among them.

| stretch | from | to | ends in | head at its end | commits after its merge base | touching | M |
| --- | --- | --- | --- | --- | --- | --- | --- |
| ACME-1, first | 09:45 | 11:00 | a conflict | `3b373d9e…` | #30 at 10:10, #12 at 10:45 | #12: it changes `shelf/stock.go` | 1 |
| ACME-2 | 09:50 | 10:45 | its merge | `805b5ecb…` | #30 at 10:10 | none: #30 changes `billing/invoice.go` alone | 0 |
| ACME-1, second | 11:30 | 12:45 | its merge | `eeca3953…` | none | none | 0 |

A stretch with M = 0 is left out of the fit, so the model is fitted on ACME-1's first stretch alone.
Every stretch it holds conflicted: the chance of a stretch with a touching merge is 1, exactly, and the chance of a stretch with none is 0 by the rule, not by the fit.
The chance rises with a touching merge from 0 to 1, and no draw's u can change an outcome.

A tool that counted every commit of the base would give ACME-1's first stretch M = 2 and ACME-2's M = 1.

## The replay under `legion-v20.42.5`, on `main+conflicts=drawn`

The recorded rule, so each segment starts at its recorded time, and the replay merges ACME-2 at 10:45, where #12's commit enters the replay's base.

- 10:45, ACME-2's stretch ends at its merge and draws.
  Its profile is `aisle/sort.go` and `shelf/stock.go`; the base holds #30's commit, which changes neither, and not yet #12's own.
  No touching merge: `merges` 0, `chance` 0, `outcome` `clean`, and ACME-2 merges.

- 11:00, ACME-1's first stretch ends, where the record ended it in a conflict, and draws.
  Its profile is `shelf/stock.go`; the base holds #30's commit and #12's, and #12's changes `shelf/stock.go`.
  One touching merge: `merges` 1, `chance` 1, `outcome` `conflict`.
  The conflict moves ACME-1 back to implementing by `conflict`; the loop's work is drawn from the recorded loops, of which there is one, ACME-1's own, so it runs as recorded: implementing 11:00 to 11:30, back in merging at 12:30.

- 12:45, ACME-1's second stretch ends at its merge and draws.
  The head that stands is `eeca3953…`, whose merge base is #12's commit, and the base took no commit after it: `merges` 0, `chance` 0, `outcome` `clean`, and ACME-1 merges.

The replay is the record.

## The scorecard under `legion-v20.42.5`, worked by hand

The span is 4 hours.

**Slots.**
ACME-1 09:00 to 10:15 (75 minutes) and 11:00 to 12:00 (60), ACME-2 09:05 to 10:20 (75): 3 slots on 2001-03-05; `firstSlot` ACME-1 09:00, ACME-2 09:05.
Each was taken in the pass where its issue began to wait (its filing; the 11:00 move back, with a slot free since 10:15), so the waits are 0, 0 and 0 and the median is 0.

**Slot-hours.**
Every slot is held in a turn: ACME-1's five 15-minute turns 09:00 to 10:15, its implementer, tester and reviewer 11:00 to 12:00, ACME-2's five turns 09:05 to 10:20.
210 minutes: `slotHoursInTurn` 3.5 hours, `slotHoursNoTurn` 0.

**In flight.**
ACME-1 09:00 to 12:45 (225 minutes), ACME-2 09:05 to 10:45 (100): 325 minutes, 5.41666… hours, an average of 5.41666…/4 = 1.354166…, at most 2.

**Done and work left.**
ACME-2 done at 10:45, ACME-1 at 12:45 (`done` 2); `workLeft` 0.

**Started work without a slot.**
ACME-1's start after the conflict took its slot in the pass that owed it: readings (a) and (b) are 0.

**Past the review.**
ACME-1 10:15 to 11:00 (45 minutes) and 12:00 to 12:45 (45), ACME-2 10:20 to 10:45 (25), none with a slot: `pastReviewNotHolding` 115 minutes, 1.91666… hours, an average of 0.479166…, and 2 at once from 10:20 to 10:45; `pastReviewHolding` 0.

**Priority taken.**
No issue waits for a slot: 0.

**Loops.**
One round of `conflict`, from the move back at 11:00 to ACME-1's return to merging at 12:30: 1.5 hours.

**Waits.**
None.

**Assumptions.**
1; 2 (both past their review); 3 (a send-back loop, and a drawn conflict); 9; 13 (the loop moves ACME-1 back); 18 (the window records a conflict).
`assumptions` is [1, 2, 3, 9, 13, 18]; `scenario` is `none`.

## The signals, worked by hand

The window holds no record of any of R11's signals: its `window.json` names no span under `signals`, and it holds no launcher's lines, no provider's record, no merge queue file and no settings events.
So at 09:00, the window's start, the run under `legion-v20.42.5` writes each signal unrecorded (`null`) in `signalValues`.
The run limits all five signals (`limits`), and run.json echoes them; the rule admits by no signal, so the limits decide nothing.

- `idle`: no span of the idle share's record, so the replay's own holders give no recorded share.
- `queue`: no merge queue file and no span of its record, so the merge queue's model is not fitted.
- `provider`: no minute of the provider's record, so the tokens model has no minute to read.
- `kills`: no launcher's lines and no span of the kills' record, so the kill model is not fitted.
- `approvals`: no span of the approvals' record, so the replay's copy of Dispatch gives no recorded count.
