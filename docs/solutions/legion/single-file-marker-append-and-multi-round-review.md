---
title: "Single-file marker append and multi-round Legion review"
category: legion
tags:
  - legion-workflow
  - exact-line-proof
  - jj-commit-hygiene
  - review-rounds
date: 2026-09-25
status: active
module: legion-worker
related_issues:
  - "S393987374-1"
  - "sjawhar/legion-smoke#295"
---

# Single-file marker append and multi-round Legion review

Reusable learnings from delivering a one-file append (`smoke.txt`) through a Legion
implement/test/review loop with three reviewer-requested correction rounds. Integrates the
implementer's own context with an independent fresh-eyes pass on the PR diff.

## Prove an exact-line append on the real surface, not just locally

Structure the proof as whole-line + fixed-string + line-number, paired with a same-line
negative control:

- `grep -Fxn '<exact line>' smoke.txt` — `-F` treats hyphens/parens literally (no regex),
  `-x` forces a whole-line match so a substring cannot false-pass, `-n` emits the citable
  `<lineno>:<line>`.
- Negative control: the SAME string plus a ` MISSING` suffix must return no match, proving the
  check discriminates rather than always passing.
- Re-assert prior-round lines still exist on later rounds.
- Drive the proof on BOTH surfaces: the tracked file at the head SHA
  (`jj file show -r <sha> smoke.txt | grep -Fxn ...`) AND the GitHub PR diff (the `+` line is
  present). A local match without the pushed diff does not prove the line reached the PR head.

## Keep the handoff commit separate from the code commit

The handoff JSON must be its own reviewable commit, never mixed into the code change. The trap:
running `jj describe` on the working copy to name the code commit, then writing the handoff into
that SAME working copy — the handoff file joins the code commit. Fix by putting the marker
append in its own change and `jj new` before writing the handoff, then
`jj split -m "<phase>: record handoff" .legion/<phase>.json`. This keeps the reviewer's diff a
single-file, `+N`-line change and keeps handoff metadata out of the reviewed surface.

## Record the pushed branch tip as the head SHA, and align it everywhere

Committing the handoff after the code commit advances the branch head, so the code-correction
commit SHA is NOT the PR head. Capture `headSha` as the last step before writing the handoff —
the bookmark target / `jj log -r @- -T commit_id` after the push — and make the PR body's E2E
line, the implement handoff, and the test handoff all cite that one pushed-head SHA. Citing the
code-correction commit while the PR body and test handoff cite the tip creates a drift a
reviewer must reconcile by hand.

## One line per round, re-proved at the corrective head

The reviewer pins the pre-correction head; the implementer appends exactly one line; the
verifier re-runs the grep + PR-diff proof against the head produced by the corrective push (not
the head the reviewer commented on) and cites that new SHA in the PR body Verification block.
Each round moving the bookmark forward (`git push` sideways/forward) keeps CI, the PR body, and
the handoffs coherent across rounds.

## Follow the spec's exact text even when it is not globally unique

When the plan fixes the literal line (here round 0's `stage-3 proof`), append exactly that text
even though `smoke.txt` holds collision-prone variants from prior issues. Do not "improve" it by
adding an issue token the spec did not ask for; disambiguate attribution by commit SHA and PR
number instead. The review-round lines that DO carry the issue token (`(S393987374)`) are
globally unique and safe to `grep -Fx`.

## Treat appends as non-idempotent and newline-fragile

Re-running an append silently duplicates a line, and if the file lacks a trailing newline the new
marker concatenates onto the last line and breaks the `-x` whole-line proof. When a step may be
re-driven, guard it: `grep -Fxq '<line>' smoke.txt || printf '%s\n' '<line>' >> smoke.txt`, and
confirm the file already ends in a newline. This makes the step safe to re-run and preserves
clean per-line diffs.
