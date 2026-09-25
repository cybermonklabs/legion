---
title: "Legion implementer: proof commands and jj handoff mechanics"
category: legion
tags:
  - legion-workflow
  - proof-discipline
  - jj
  - handoff
  - review-rounds
date: 2026-09-25
status: active
module: legion-implementer
related_issues:
  - "S392225819-1"
  - "sjawhar/legion-smoke#290"
---

# Legion implementer: proof commands and jj handoff mechanics

Reusable lessons a Legion implementer will hit again, drawn from PR
`sjawhar/legion-smoke#290` (issue `S392225819-1`). The code change was one appended
line plus three review-round lines, but the phase workflow surfaced concrete,
transferable pitfalls in proof discipline and `jj` commit shaping.

## Match the proof command to the acceptance predicate, not to the plan's convenience

Acceptance was "smoke.txt **contains** the line `Stage 3 smoke proof.`" — a containment
predicate. The plan recommended `tail -n1 smoke.txt` equality. That works only while the
proof line is the last line. The spec permitted one review-round line per round, and three
rounds appended `Stage 3 review round 1/2/3 (S392225819)` **after** the proof line, so
`tail -n1` would report the round-3 marker and fail despite acceptance being met.

Pivot the proof command to the acceptance semantics:

```bash
grep -Fxq 'Stage 3 smoke proof.' smoke.txt   # containment, not position
```

When a spec permits later appends, assume your proof line will move off the tail and prove
containment from the start.

## Use fixed-string, whole-line matching so the negative control has meaning

`smoke.txt` already held near-miss lines: `Stage 3 smoke proof` (no period) and
`Stage 3 smoke proof line.`. A substring `grep -F` or a regex would match those and "pass"
without the change. `grep -Fx` (fixed string + whole line) makes the trailing period and the
line boundary discriminating, which is exactly what gives the `Stage 3 WRONG` negative
control its force. A negative control is only worth citing if the positive check would
actually fail on a wrong input.

## Cite a real branch commit for `proof.headSha`, and prefer the pushed head

The `Proof.headSha` must resolve to a commit that is on the branch the reviewer and CI
evaluate. In `jj`, the working-copy commit `@` and the pushed bookmark head diverge —
especially once a `implement: record handoff` commit rides on top of the code commit. Two
rules:

- Put the code commit **below** and the handoff commit **on top** as the branch tip. The
  code commit is then a stable ancestor whose SHA does not change when you rewrite the
  handoff commit. If you instead `jj split .legion/<phase>.json` out of a mixed `@`, split
  makes the handoff the **parent** and rewrites the code commit's SHA — invalidating any
  `headSha` you already recorded. Reorder with
  `jj -R "$LEGION_WORKSPACE" rebase -r <handoff-commit> -d <code-commit>` so code is the
  stable ancestor.
- Prefer citing the pushed head (`gh pr view <n> --json headRefOid`, or the Contents API
  `?ref=<sha>`) so your SHA matches what CI ran and the reviewer reviewed. Citing an
  ancestor code commit is defensible (the change lives there and it is on the branch), but
  the head is unambiguous.

## Keep the working copy clean with the handoff at `@-` for `handoff_complete`

`legion handoff complete` refuses when the handoff file has uncommitted changes in the
working copy. If `@` **is** the handoff commit (the handoff file lives in the working-copy
commit), it errors:

```
.legion/<phase>.json has changes in the working copy that are not committed
```

Fix: after committing the handoff and pushing, run `jj -R "$LEGION_WORKSPACE" new` so `@`
is an empty working copy on top and the handoff commit sits at `@-`. Land the code edit as
its own commit, the phase handoff as a separate commit on top, then an empty `@`.

## Every corrective review round is a full implement phase

A push does not finish a round. Each round: append the requested change as its own code
commit, write a fresh implement handoff (with a new proof at the new head), commit it, push
(advancing the bookmark forward — this also carries the tester's own `test: record handoff`
commit, which the tester adds but never pushes; only the implementer pushes), then call the
`legion` tool's `handoff_complete`. The tester re-runs after each round's completion.

Also run `legion threads resolve --pr <n> --repo <owner>/<repo>` before every corrective
push even when there are no inline threads: a `CHANGES_REQUESTED` review can be a top-level
body with zero `reviewThreads`; the command prints `no unresolved threads` and that output
goes into the PR body's `Threads` section.

## For a no-logic repo, the file surface is the production-like surface — drive it two ways

With nothing to compile or run, the acceptance artifact itself is the production-like
surface. The implementer reads it back from the local `$LEGION_WORKSPACE` tree; the tester
independently re-drives it from the authoritative remote at the exact pushed head
(`gh api repos/<owner>/<repo>/contents/smoke.txt?ref=<sha> --jq .content | base64 -d | grep -Fxq …`)
rather than trusting the implementer's local check, plus CI green (check + gate) at that
head.

## Expect CI to double-run per head

Each pushed head triggered two `ci` runs and two `fail-on-demand` runs (push + pull_request
events). Watch all of them or filter by `headSha`, and cite one run id per workflow in the
PR body's `CI` line.
