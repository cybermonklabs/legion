---
title: "Legion append-style / docs-only proof workflow mechanics"
category: legion
tags:
  - legion
  - jj
  - handoff
  - e2e-proof
  - review-rounds
date: 2026-09-25
status: active
module: legion-phase-worker
related_issues:
  - "S392083388-1"
  - "sjawhar/legion-smoke#197"
---

# Legion append-style / docs-only proof workflow mechanics

Reusable learnings from a Legion phase-worker run whose product change was trivial
(append plain-text marker lines to a single file across an initial round plus three
review rounds). The value is entirely in the workflow mechanics, not the text.

## The handoff-commit head-SHA self-reference is structural

A phase handoff records the head SHA, but committing the handoff advances the head to the
handoff commit itself. A file can never contain its own commit's SHA, so any headSha the
handoff records is stale the instant it is pushed.

- Set the implementer handoff `headSha` to the **implementation commit** (the code/content
  commit), and keep that commit an **ancestor** of the handoff commit. Then the recorded
  SHA is stable and non-circular, and it names exactly the commit that carries the change.
- Do **not** try to record the current PR head (the handoff commit's own SHA) inside the
  handoff. Cite CI run ids at the true pushed head in the **PR body** (which is not
  committed), and let the downstream phase (tester) re-derive and verify CI green at the
  actual current PR head. Never gate merge on the implementer's own recorded SHA.

## jj: build implementation-then-handoff as separate commits without reordering

`jj split .legion/<phase>.json` from a working copy that **already contains the code change**
pulls the handoff into the *parent* and leaves the code change as the *child*, reordering the
chain, changing the implementation commit's SHA, and orphaning a previously-pushed head.

Clean sequence instead:

1. Make the code edit in `@`; `jj describe -m "<impl message>"` — this is the implementation
   commit, and its SHA is now stable.
2. `jj new` to get an empty working copy on top.
3. Write the handoff file there (referencing the implementation commit SHA from step 1),
   `jj describe -m "<phase>: record handoff"`.
4. `jj bookmark set legion/<KEY> -r @` and push.

## `handoff_complete` requires a clean working copy

The `legion` tool refuses `handoff_complete` when the handoff file shows as an uncommitted
working-copy change — e.g. after `jj edit`-ing onto the handoff commit to fix it. After the
handoff is committed, run `jj new <handoff-commit>` so the working copy is a clean empty
commit before calling `handoff_complete`.

## E2E proof for a docs-only / non-code change

There is no runtime to exercise, so the proof is the real user-facing remote surface plus a
negative control — not a unit test:

- Positive: fetch the file through the GitHub contents API on the branch ref and observe the
  expected bytes (`legion gh -- api repos/<owner>/<repo>/contents/<file>?ref=<branch> --jq
  '.content' | base64 -d`).
- Negative control: the same probe against a nonexistent ref returns `404 No commit found for
  the ref` — proving the probe would fail on a bad input.

## Cite CI by run id bound to the exact pushed head

Every push — the code push **and each handoff commit** — re-triggers the required checks and
produces a new run generation. A green check at a superseded head is not proof the current
head is green. Cite the run ids whose commit is the current PR head, and treat runs at earlier
heads as superseded (a useful negative control in the proof).

## Review rounds extend the same PR; the round counter advances, not the PR

Each `CHANGES_REQUESTED` round is its own implementing phase: append to the same file, make a
new correction commit, write a new implement handoff (round N), push the branch forward, let
CI re-green at the new head, run `legion threads resolve`, refresh the PR body, then
`handoff_complete`. Do not open a new PR or branch per round. The prior round's tester handoff
commit rides the implementer's next push.

## Thread bookkeeping must reflect what the reviewer actually left

A top-level `CHANGES_REQUESTED` review with no inline comments has **zero** review threads to
resolve. `legion threads resolve --pr <n> --repo <owner>/<repo>` prints `no unresolved
threads` (exit 0); record `0 resolved, 0 unresolved` — never fabricate thread dispositions.

## Keep phase handoffs under `.legion/` to preserve a one-file merge diff

When the spec requires exactly one changed file on `main`, the `.legion/*.json` handoffs are
still committed on the branch; the one-file invariant is preserved because `.legion/` is
deleted before the human merge (a dedicated, deletable directory). Always verify the target
file exists at HEAD first and stop-and-report if it is missing — never create a substitute.
