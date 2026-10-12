---
title: "Recompute PR body claims from the API at every edit; name heads, never 'every head'"
category: legion
tags:
  - pr-body
  - evidence
  - implementer
  - review-threads
  - ci
date: 2026-10-12
status: active
module: legion-phase-workers
related_issues:
  - "LEGSMOKE-692"
  - "sjawhar/legion-smoke#559"
---

# Recompute PR body claims from the API at every edit; name heads, never 'every head'

A PR body line that summarises history goes stale at the next push. Before each body edit:

- Re-derive `Size:` from `gh pr view <n> --json changedFiles,additions,deletions` and
  `gh pr diff <n> --name-only` at the head you are stamping; list the handoff files by name, since
  each phase adds one and retro removes them all.
- In `CI:` and `Change:`, name each head and its runs (`<sha>`: `check` run ids, `gate` run ids,
  conclusion). Never write "green on every code head": a fixture or a red round makes it false
  for the branch's history even when it is true for your own heads. State which head is red and why.
- Before any push, run the path guard against the base,
  `jj diff --from <base>@origin --to @- --name-only`, and compare it with the set the acceptance
  allows: it is the read that proves an added-then-deleted fixture (`.fail-me`) nets out of
  `base...head` after a forward removal commit, before GitHub shows it; confirm with
  `gh pr diff <n> --name-only` after the push.
- Resolve a thread (`resolveReviewThread` → `isResolved: true`) before you write its `Threads:`
  row, so the row records a fact, and stamp the row with the head you just pushed; a bot thread the
  reviewer accepted gets its row from the reviewer's Envoy message (reply id, node id) the same way.

## Evidence

LEGSMOKE-692 (sjawhar/legion-smoke#559): review round 1 recorded two minor findings against the
body, not the diff — `Change:` bullet 3 claimed both workflows "green on every code head" while
`gate` had failed ×2 at the driver's `.fail-me` head `2573393a` (runs 38223123223, 38223127496),
and `Size:` named two handoff files when the diff carried three, then four. Both were rewritten
from the live API at the append round (`changedFiles` 5, `+288/−0`; per-head run ids for
`f182c812`, `884090cc`, `e53bb94d`; the fixture head named as red). The pre-push guard against
`main@origin` before pushing `884090cc` listed only `smoke/LEGSMOKE-692.md` and `.legion/LEGSMOKE-692/*`
— no `.fail-me` — and `gh pr diff 559 --name-only | grep -c '^\.fail-me$'` → `0` confirmed it after.
