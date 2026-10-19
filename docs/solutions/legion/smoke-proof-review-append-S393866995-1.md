---
title: "Legion one-file smoke proofs: filenames, round-append lines, and content-grep proof"
category: legion
tags:
  - smoke-proof
  - review-rounds
  - proof-surface
date: 2026-10-18
status: active
module: legion-smoke
related_issues:
  - "S393866995-1"
  - "sjawhar/legion-smoke#627"
---

# Legion one-file smoke proofs: filenames, round-append lines, and content-grep proof

A Legion smoke proof adds one file `smoke/<KEY>.md`. Follow these rules so the acceptance grep,
the review-append loop, and the proof surface all agree.

- Name the file `smoke/<KEY>.md` with the full suffixed key (e.g. `S393866995-1`); write that full
  key in the title line, but write the base key with no `-N` suffix in every appended round line's
  parenthetical: `Legion proof review round N (S393866995)`.
- Settle the spec-vs-convention append count before implementing. When the spec says a review "may
  ask for one more line" but the convention appends one line per review round, reconcile it up
  front with the architect; do not plan for a single appended line and then discover the loop runs
  multiple rounds.
- Each review round appends exactly one new line and changes nothing else. Prove it with a
  line-count assertion (`wc -l` == 1 + round number) plus a negative-control grep for the
  not-yet-present next round (`grep -q 'review round N+1'` exits 1), not a bare "line exists" grep.
- Prove the criterion on the git-repository working tree at the PR head SHA, never from CI. The
  smoke repo's CI is a no-op gate (`ci.yml` sleeps, `fail-on-demand.yml` passes without `.fail-me`),
  so green CI says nothing about file content. Grep the exact text the reviewer dictated, with a
  discriminating negative control (a bogus key such as `S000000000-0` exits 1).

## Evidence

- PR sjawhar/legion-smoke#627, issue S393866995-1. Final file: title `S393866995-1 — Primary
  durable workflow profile` plus `Legion proof review round {1,2,3} (S393866995)` — full key in
  the title, base key in each round line, matching siblings S394802790-1 and S394543611-1.
- The plan's `scopeAppendClause` predicted "append exactly one line"; the review loop ran three
  rounds and appended three lines. The gap was flagged in `gapAnalysis` but deferred rather than
  reconciled, so the prediction was wrong.
- Per-round proofs asserted `wc -l` = 2/3/4 at distinct head SHAs, each with a negative control
  greping the next round line (`round 2`/`round 3`/`round 4`, exit 1); the implementer/tester read
  CI rather than re-running it.
- Sibling Stage 3 proofs are not uniform on the round phrase: S394570180-1/S393569515-1/S394058563-1
  use `Stage 3 review round N`, this PR and S394802790-1 use `Legion proof review round N`. Match
  the reviewer's dictated text, never a sibling's wording blindly.
