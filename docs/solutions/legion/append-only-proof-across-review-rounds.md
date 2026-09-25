---
title: "Append-only proofs across Legion review rounds: single-file invariant, handoff-commit head drift, and body-only review requests"
category: legion
tags:
  - legion-workflow
  - jj-split
  - review-rounds
  - product-diff-fingerprint
  - proof-discipline
date: 2026-09-25
status: active
module: legion-worker
related_issues:
  - "S392225649-1"
  - "sjawhar/legion-smoke#175"
---

# Append-only proofs across Legion review rounds

Reusable mechanics observed implementing S392225649-1 (a Stage 3 workflow proof: append proof
lines to `smoke/stage3.txt` across an initial implement plus three review rounds, all on one
branch / PR #175). The product content was trivial; the durable value is the Legion workflow
mechanics that generalize to any multi-round implement phase.

## `jj split` handoff commits move the branch head — re-resolve the live SHA, never trust a recorded one

Each phase drops its handoff into `.legion/<phase>.json` and commits it with
`jj split -m "<phase>: record handoff" .legion/<phase>.json`. Splitting the handoff out of the
working-copy commit reorders history: the handoff becomes a separate commit and the product
commit's SHA changes. The branch head therefore differs from any SHA an earlier tool call
observed.

- Set the implementer proof's `headSha` from a fresh `legion gh -- pr view <n> --json headRefOid`
  **after** the push, not from the pre-split working-copy id.
- Downstream phases (tester, reviewer) must re-query the live head and pin the concrete SHA
  (here `bdb71bffc644…`) rather than copying a placeholder from a predecessor handoff.

## Verify the *product* diff with the fingerprint subtraction, not the raw PR diff

Every Legion issue branch legitimately carries `.legion/*.json` handoff artifacts, so the raw
`pr diff` is multi-file even when the product change is one file. "Single-file" claims are only
meaningful after excluding process paths with the fingerprint set `~(.legion | docs/solutions)`:

```bash
legion gh -- pr diff <n> --repo <owner>/<repo> --name-only | grep -v '^\.legion/'
# → exactly the product files; here only smoke/stage3.txt
```

Do not flag `.legion/` churn as a product regression, and do not assert a clean single-file diff
from the raw output alone.

## Review rounds can arrive as CHANGES_REQUESTED review *bodies* with zero inline threads

All three rounds delivered their "append one more line" request in the review body, not as inline
review threads. Consequently `legion threads resolve --pr <n> --repo <owner>/<repo>` correctly
prints `no unresolved threads`, and the handoff records `threads: resolved 0 / unresolved 0`. An
empty thread-resolution result here is expected, not a skipped step: proof-of-address is the
appended line plus a green re-review, and the PR body's `Threads` section names each round's
request and the commit that fixed it instead of per-thread dispositions.

## Corrective rounds ride the same branch and PR — one appended line each

The plan pre-authorized "a review may ask for one more line appended … on the same branch". Each
round is exactly one more appended line below the prior lines, changing no other file, committed
on the existing bookmark `legion/<KEY>` and pushed to the same PR. This keeps the product diff
monotonically growing by one line per round and preserves the single-file invariant across N
rounds. Never open a new PR per round.

## The plan's AC1 is the base case; its follow-up clause governs review rounds

`plan.json` AC1 said "PR adds exactly one line", but the delivered workflow appended four lines
across three rounds. The reconciling authority is the plan's in-scope follow-up concern, not the
literal AC1. Treat AC1 as the trivial base case and the follow-up clause as the governing rule;
do not fail the PR for "more than one added line".

## Dual-proof discipline with an explicit negative control is what makes the proof credible

The implementer records the observed added lines **and** a negative control (the lines absent on
`main` pre-merge via the contents API); the tester independently re-runs the identical
`pr diff` command, confirms the output matches, and re-checks `main`. The tester's verdict is
`verified` only after reproducing — not merely reading — the implementer's proof. Replicate:
same command, independent run, explicit pre-merge-absence control.

## Ordering caveat: retro reached with `.legion/` still on the branch head

In this smoke run the merge-gate step that deletes `.legion/` (the implementer's cleanup push at
the reviewer's direction, then re-approval of that cleaned head) was not performed before retro:
the reviewer's APPROVE landed on head `bdb71bffc644`, which still carries the `.legion/*.json`
handoffs. That is safe **only** because the product-diff fingerprint excludes
`~(.legion | docs/solutions)`. For a real feature, confirm the `.legion/` deletion push and its
re-approval happen before READY, or that the merge strategy strips `.legion/` — otherwise process
artifacts accumulate on `main`. Retro itself writes no `.legion` file and adds only this
`docs/solutions/` document, so it does not re-dirty the tree.
