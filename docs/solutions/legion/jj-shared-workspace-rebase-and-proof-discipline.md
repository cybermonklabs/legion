---
title: "jj shared-workspace conflict resolution and proof discipline for Legion phase workers"
category: legion
tags:
  - jj
  - shared-workspace
  - rebase
  - conflict-resolution
  - acceptance-criteria
  - proof
date: 2026-09-27
status: active
module: legion-phase-worker
related_issues:
  - "S390636892-1"
  - "sjawhar/legion-smoke#380"
---

# jj shared-workspace conflict resolution and proof discipline for Legion phase workers

Learnings from delivering S390636892-1 (PR #380): a one-file smoke marker
`smoke/S390636892-1.md` (first line = issue key, then one appended
`Stage 3 review round N (...)` line per review round) driven through the full
plan -> implement -> test -> review x3 -> retro pipeline on a jj shared-workspace clone.

## 1. A `.legion/` modify/delete conflict is resolved by a whole-chain rebase, not per-file surgery

Round 0 opened MERGEABLE, then GitHub flipped it to CONFLICTING: `main` (a proof fixture) had
**deleted** prior-run `.legion/` handoffs, while this branch's `plan: record handoff` commit
**modified/re-added** `.legion/plan.json`. That is a modify/delete conflict confined entirely to a
process artifact.

Resolution that worked, forward-only (never `jj undo` / `jj op restore` — those rewrite the shared
operation log for every issue workspace of the clone):

```
jj rebase -s 'roots(main@origin..@)' -d main@origin   # whole chain, so tester/reviewer commits move too
jj new <conflicted-plan-commit>                        # child of the first conflicted commit
jj restore --from <original-plan-commit> .legion/plan.json   # keep THIS run's handoff
jj squash                                              # fold resolution into the plan commit; descendants rebase
```

Trap that cost a step: after `jj new <plan-commit>` + `jj squash`, `@` stayed a child of the plan
commit, so the impl commit became a **sibling** of `@`, not its ancestor — `main@origin..@` no
longer contained the product change. Re-anchor with `jj new <impl-commit>` before setting the
bookmark and pushing.

## 2. Prove a rebase is product-neutral with a fingerprint that excludes `.legion` and `docs/solutions`

The unchanged-diff fingerprint (`jj diff --from fork_point(...) --to <head> --git --context 0
'~(.legion | docs/solutions)' | sed -e '/^@@/d' -e '/^index /d' | sha256sum`) was **identical**
before and after the rebase (`da94d80e...` -> `da94d80e...`). Because it ignores process artifacts,
an equal hash is hard proof that only `.legion` churned — which is exactly what lets a rebase count
as a *confirmation, not a new review round*, and lets each downstream role compare against its own
last-verified head. Post one PR comment: `rebase <old> -> <new>; fingerprint <a> -> <b>; unchanged`.

## 3. Committing `.legion/*.json` into the product tree is safe ONLY because both the fingerprint and the single-file AC diff-scope exclude it

The PR literally ADDs four `.legion/*.json` files, yet AC2 ("PR diff touches only that one file")
still holds — because the diff-scope check is run with `'~.legion'`. A future worker who drops that
exclusion would see a 5-file diff and wrongly fail the single-file contract. Keep the exclusion in
every scope/fingerprint check, as one union expression `'~(.legion | docs/solutions)'` (two separate
`'~.legion' '~docs/solutions'` args select everything and exclude nothing in jj).

## 4. Assert positional invariants for a file that grows across rounds — never whole-file equality

Correctness gap found in retro (fresh-eyes): the plan's authoritative AC1 command was
`test "$(cat smoke/S390636892-1.md)" = 'S390636892-1'`. That passes only on a *single-line* file and
would **falsely fail** at the final 4-line head after three review-round appends. The implementer
correctly delivered AC1 as `test "$(sed -n '1p' ...)" = 'S390636892-1'` (first-line, head-independent)
and drifted AC2's threshold from the plan's `-ge 2` to `-ge 4` as rounds accrued. Behaviour was
correct, but a worker copying the plan's AC1 verbatim would get a false negative.

Lesson: when a file is specified to grow over rounds, the plan must state **positional** assertions
(`sed -n '<line>p'`) from the start, and the delivered proof commands must match the plan's — or the
divergence itself is a durable learning.

## 5. For a proof marker, the one-file constraint IS the contract

AC2's negative control ("touching any other file fails the single-file diff check") turns scope
creep into a test failure rather than a review nicety. Encode the scope bound as an executable check,
not prose.
