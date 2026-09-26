---
title: "Reading product truth in a Legion PR whose diff carries .legion handoffs"
category: legion
tags:
  - legion
  - jj
  - pr-diff
  - e2e-proof
  - review-rounds
date: 2026-09-26
status: active
module: legion-worker
related_issues:
  - "S394547882-1"
  - "sjawhar/legion-smoke#333"
---

# Reading product truth in a Legion PR whose diff carries .legion handoffs

Every Legion phase commits its own `.legion/<phase>.json` handoff onto the issue branch, so
the branch tip and the raw PR diff always carry process artifacts alongside the real change.
On S394547882-1 (PR #333, a docs-only append to `README.md` driven through
plan -> implement -> test -> three review rounds -> retro) that gap between "what the PR diff
shows" and "what the change actually is" was the single recurring source of ceremony. These are
the transferable lessons.

## The `.legion/*.json` handoffs pollute the raw PR diff — filter them before asserting scope

`gh pr diff` on #333 lists five changed files (four `.legion/*.json` adds + `README.md`), but
only `README.md` is a product change. The acceptance criterion "changes no other file" is about
product files; the handoffs are process artifacts removed at the end of a clean review.

Assert scope with the product-only fileset, never the raw diff:

```
jj diff --from main@origin --to <head> --git --context 0 '~.legion'
```

A worker who diffs without `'~.legion'` will mis-report scope and wrongly claim the PR touches
non-target files. This exact command belongs in both the `E2E (implementer)` and `E2E (tester)`
proof lines. Note the single-expression fileset: `'~(.legion | docs/solutions)'` is required
when a `docs/solutions` retro commit is also present — two separate `'~x' '~y'` args union and
exclude nothing.

## The branch tip SHA is not the product head — anchor proofs to the product head

Because each phase advances the tip with a handoff commit, the implementer's product head
(`0594f4c5`), the tester's tip (`81519b87`), and the reviewed head all differed *only* by
`.legion/*.json` commits. Anchor acceptance evidence to the product head, state the
tip-vs-product delta explicitly ("differs only by the `.legion/…` process artifact"), and do not
be alarmed when the tip SHA in `pr view` does not match the SHA your product diff was taken at.

## Legion review rounds are REQUEST_CHANGES review bodies, not inline threads

All three change requests on #333 arrived as review-body findings, so
`legion threads resolve --pr 333` correctly reported `no unresolved threads` and the PR body
showed `Threads: 0 resolved, 0 unresolved` — despite three rounds of change. Do not read an
empty thread-resolve result as "nothing was requested." Read the review *decisions/bodies* for
findings and record which fix commit answered each round in the PR body's Threads section
(here rounds 1/2/3 -> `9359bcb3` / `e57321d4` / `0594f4c5`).

## Pre-authorizing the anticipated review edit in the plan keeps iterative churn cheap

The plan handoff pre-approved "add exactly one more appended line per review round in the same
PR" and drew a hard escalation boundary ("any request to change a file other than README.md is
out of scope -> escalate to the architect"). That turned each REQUEST_CHANGES into a mechanical
append instead of an architect round-trip. For scripted or predictably iterative review flows,
encode the expected in-scope correction *and* its out-of-scope boundary in the plan handoff.

## Negative controls that point at a real prior commit catch more than a positive re-run

The tester's strongest evidence was a differential negative control: diffing an actual earlier
commit (`49cbc0a8`, before the round-3 line) and showing zero occurrences of the round-3 string.
That catches a check that would always pass. By contrast, independently re-running the identical
positive product-only diff mostly re-derived the implementer's result. Anchor negative controls
to real prior commit SHAs; for trivial docs changes, a tester's re-run of the same positive
command is low-yield.
