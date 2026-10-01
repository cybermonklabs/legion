---
title: "Implementer mechanics on a Legion phase branch: credential windows, plan-predicted checks, and push ordering"
category: legion-workflow
tags:
  - legion
  - implementer
  - legion-gh
  - grant-expired
  - jj-split
  - plan-verify
  - push-ordering
  - review-round
date: 2026-10-01
status: active
module: smoke
related_issues:
  - "LEGSMOKE-372"
  - "sjawhar/legion-smoke#477"
---

# Implementer mechanics on a Legion phase branch: credential windows, plan-predicted checks, and push ordering

Learned on LEGSMOKE-372 (Stage 4b proof tree 1), pull request sjawhar/legion-smoke#477: one new
file `smoke/stage4b-legsmoke-372.md`, one line, 39 bytes, proved on GitHub through `legion gh --`
by the implementer, the tester, and the reviewer. Everything below was observed in that run;
anything inferred is marked. The LEGSMOKE-366 retro already covers proving at the code commit and
re-checking at the handoff head, handoff paths polluting an "exactly one path" check, the four CI
runs an `on: [push, pull_request]` workflow pair produces per head, and the zero-run
`skip-checks` head; this document adds what that run did not hit.

## One `legion gh` grant lives 60 seconds: a `--watch` and its follow-up calls cannot share a bash call

`legion gh -- pr checks 477 --watch` took 75 s the first time and 95 s the second. In the first
case it was the only command in its bash call and the next bash call's `legion gh -- run list`
succeeded. In the second case `run list` followed the watch inside the same `&&` chain and failed:

```
legion gh: Unable to redeem LEGION_GRANT: daemon returned 403: {"code":"GRANT_EXPIRED","error":"grant expired"}
```

The extension writes the grant file once per bash call, before the call starts, and a grant is
redeemable for 60 s. The watch itself redeems once at its start and then runs on the token it got,
so it finishes; every `legion gh` after it in the same call redeems against the stale grant. Put a
`--watch` (or any `gh run watch`) in a bash call of its own, and run the evidence-collecting calls
in the next one. The same shape applies to anything that may outlast a minute before the next
credentialed command: `sleep`, a long `jj` operation, a build.

## A plan's predicted byte offset is a prediction, not the contract; the worker reports the miss and keeps the content

Plan task 1's verify step said `cmp smoke/stage4b-legsmoke-372.md smoke/stage4b-legsmoke-366.md`
"differs at char 37 only". The content contract was the 39 bytes
`Stage 4b workflow proof: LEGSMOKE-372.` plus one newline. `cmp -l` printed two rows,
`36 67 66` and `37 62 66`: `372` and `366` share the leading `3` and differ in two digits, so the
first difference is at byte 36, not 37. The deep-worker that wrote the file did not bend the
content to the mispredicted check; it reported the mismatch as a departure, and the implementer
recorded it in `.legion/implement.json` `localChecks` and in the report to the architect.

For a planner: do not hand-count an offset in a verify step. Either compute it against the real
precedent (`cmp -l precedent new` while writing the plan) or write the key-agnostic form the
LEGSMOKE-366 retro recommends, `sed 's/366/372/' precedent.md | cmp - new.md`, which prints
nothing when the only difference is the key. For a worker: when a plan's incidental prediction and
its content contract disagree, the content contract wins and the disagreement is reported, never
silently repaired on either side.

## The implement handoff forces two pushes; a plan that says "commit, write the handoff, push" is not executable as written

Plan task 2 read: commit the file, write `.legion/implement.json` with its `proof` array, split the
handoff, then `legion push`. The `proof` array must name a head SHA at which CI has settled, and
CI cannot have settled at a commit that is not yet on GitHub. The executable order was:

1. `jj split -m "smoke: …" smoke/stage4b-legsmoke-372.md`; `legion push` (bookmark
   `7fe02476 → 716591d1`; CI runs in full).
2. `legion gh -- pr create …` with the READY body; `legion gh -- pr checks 477 --watch` until the
   four runs settle; run the criterion commands at `pr view --json headRefOid` (`716591d1`).
3. `handoff_write` with that proof; `jj split -m "implement: record handoff" .legion/implement.json`;
   `legion push` (bookmark `716591d1 → ff901e4c`). `implement.json` is not on `legion push`'s
   skip-CI list, so this push runs CI in full again.
4. Wait for CI at `ff901e4c`, then edit only the PR body's `CI` line to add that head's four run
   ids beside the code head's, stating that the smoke file is the same blob (`ae326ebf3f`) at both.

A planner writing an implementer task list should spell out steps 1–4 as two pushes with the proof
between them, and should name which head the `proof` array will cite (the code commit) and which
the PR body will additionally cite (the handoff head). The tester then expects the handoff head and
verifies at both; on this run it did, and recorded `implementerProof.verdict: verified`.

## `jj split` on a fully selected working copy prints a "remaining" change id that never exists

Splitting the working copy with every change selected printed:

```
Warning: All changes have been selected, so the original revision will become empty.
Selected changes : uwlvqvyn 716591d1 smoke: add Stage 4b workflow proof for LEGSMOKE-372
Remaining changes: tykmklvq 858da7d8 (empty) (no description set)
Working copy  (@) now at: lnpkxsxq fc1859ac (empty) (no description set)
```

`jj log -r tykmklvq` then answered `Revision "tykmklvq" doesn't exist`, and
`jj log -r 'descendants(7fe02476)'` showed a linear chain `plan → code → @`. The "remaining"
revision is reported and then dropped by jj itself because it is empty; the new empty `@` is a
different change id. Treat the message as noise and confirm with `descendants(<parent>)`; do not
reach for `jj abandon` to "clean it up" (the phase-worker extension refuses it, and the shared
operation log makes it dangerous even where it would run).

## A review round can decide nothing and still end in approval on the same head, with CI run in full on the way

The reviewer's first GitHub submission at `23f31c9c` was `COMMENTED` (01:04:54Z), by the proof
driver's direction that the round decide nothing; its second, `APPROVED` (01:06:59Z), was a
separate review object on the identical head, and the approval text said so ("Head unchanged since
that review"). Two facts about that path a future reviewer or merger can rely on:

- A `.legion/review.json` whose `verdict` is anything but `changes_requested` is not on
  `legion push`'s skip-CI list, so the review-handoff push ran CI in full at `23f31c9c` (four
  `success` runs, no `skip-checks` trailer), and the approval therefore sat on a head with its own
  green runs, not a carried verdict.
- The tester's handoff-only push `ce605798` did carry `skip-checks: true` and had zero check runs.
  The reviewer did not trust the carried verdict on the trailer alone: it read
  `repos/<r>/commits/ce605798` to confirm the parent was `ff901e4c` and the only change was
  `.legion/test.json added 48 0`, then cited `ff901e4c`'s four runs as `carriedFromHead`. Do the
  same parent-and-paths check before accepting any zero-run head as green.

## Commit author names the App, not the phase

On this branch the plan handoff, the test handoff, and the review handoff are all authored and
committed by `legion-reviewer[bot]`; the code commit and the implement handoff by
`legion-implementer[bot]`. Legion has two GitHub Apps, not one per role, so `author.email()`
cannot tell a planner's commit from a tester's. The reliable keys are the file path
(`.legion/<phase>.json`) and the handoff's own `phase` field; the identity check before a push is
"my role's App on both columns of my commits", never "a distinct identity per phase".
