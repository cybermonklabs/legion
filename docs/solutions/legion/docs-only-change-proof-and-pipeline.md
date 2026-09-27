---
title: "Proving and shepherding a docs-only change through the Legion pipeline"
category: legion
tags:
  - legion-workflow
  - proof-discipline
  - docs-only
  - handoffs
  - review-rounds
date: 2026-09-27
status: active
module: legion
related_issues:
  - "S394058563-1"
  - "sjawhar/legion-smoke#393"
---

# Proving and shepherding a docs-only change through the Legion pipeline

A docs-only, one-file change (here: add `smoke/S394058563-1.md`, then append one line per
review round) still runs the full Legion daemon pipeline — plan → implement → test →
review(×N) → retro. The change is trivial; the workflow mechanics are the reusable part.

## Prove a change on a surface that has no runtime

A Markdown file has no executing surface, so the proof surface is **the repository file surface
at the branch head**, verified with jj against a negative control — never skipped for lack of a
runtime.

- Positive, at the exact content commit: `jj file show -r <content-sha> <path> | wc -l`
  (line count), `jj diff -r <content-sha> --git` (must be a single hunk adding exactly the
  intended line, e.g. `@@ -1,3 +1,4 @@`), `jj diff -r <content-sha> --summary` (must equal
  `M <path>` / `A <path>` — that file only, nothing else).
- Negative control: `jj file show -r main@origin <path>` → `Error: No such path` proves the
  file is absent on `main` before merge. Pair every positive proof with an absence/refusal
  control like this.

Both the implementer's `E2E (implementer)` line and the tester's `E2E (tester)` line carry this
shape; the tester re-runs the implementer's commands independently and confirms the added text
matches the request verbatim.

## Handoff commits advance the head — pin proof to the content commit, not "head"

Every phase commits its `.legion/<phase>.json` handoff, so the branch head moves after each
phase. On this issue the implement proof was taken at content commit `6e717d15`, but by the time
the test handoff landed the head was `ff012467`, and the reviewer approved at `25e14733` — three
different SHAs. Pin diff/proof commands to the specific **content** commit
(`jj diff -r <content-sha>`), and expect a later handoff commit to sit above your proof SHA.
This is why the push that carries only a handoff (planner `plan.json`, tester `test.json`,
`changes_requested` reviewer round) skips CI, while any push carrying code runs it in full.

## Record skips; never leave them silent

On a docs-only diff, `ce-simplify-code` does not run and the `thermonuclear-deep-review` /
`thermonuclear-code-quality` pair is skipped (per `legion-worker`). Say so with a reason:
`plan.json.requiredSkills` set each phase to `"none: … docs-only …"`, and the PR body omits the
`Thermo` line. A skip with a stated reason reads as a deliberate decision to downstream phases
and retro; an unexplained absence reads as a gap.

## Review findings can be review-body comments, not anchored threads

The three review rounds were `CHANGES_REQUESTED` posted as review **bodies**, not inline
threads — `reviewThreads` was empty, so `legion threads resolve` had nothing to act on. Dispose
each finding individually anyway: a PR comment naming the fix commit and the
single-hunk/`--summary` evidence (`Fixed in <sha>: …`), and record it in the PR body's
`Threads` section. Match the reviewer-requested text exactly (verbatim), and honor the spec's
round cap — the scripted final `APPROVE` noted the instruction "takes precedence over waiting
for another review round."

## `.legion/` is present during retro; its deletion is a later merge-gate action

At retro time all four `.legion/*.json` handoffs are still committed at the branch head — retro
must read them, and retro itself writes **no** `.legion` file (only `docs/solutions/` + one
Dispatch message). Per `legion-worker`/`legion-retro` merge-gate ordering the implementer pushes
the `.legion/` deletion at the reviewer's direction and the reviewer approves that cleaned head
*before* retro. A phase worker should therefore (a) rely on the handoffs being present during
retro, and (b) not assume the branch head is free of handoff artifacts — verify the deletion
actually ran before merge rather than assuming a clean tree.
