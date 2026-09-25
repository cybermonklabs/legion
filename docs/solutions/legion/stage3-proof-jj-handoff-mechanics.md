---
title: "Legion Stage 3 proof: jj commit and handoff mechanics for a phase worker"
category: legion
tags:
  - legion-workflow
  - jj
  - handoff
  - pull-request
  - review-threads
date: 2026-09-25
status: active
module: legion-worker
related_issues:
  - "S394108011-1"
  - "sjawhar/legion-smoke#224"
---

# Legion Stage 3 proof: jj commit and handoff mechanics for a phase worker

Running the Legion plan -> implement -> test -> review -> retro workflow to completion on a
shared `jj` issue workspace surfaces several mechanical traps that are independent of the product
change. These learnings come from PR #224 (a plain-text append to `smoke.txt`) but apply to any
Legion phase worker.

## Head-SHA churn from interleaved handoff commits

The implementer commits the code change, then records the handoff in a **separate** commit
(`implement: record handoff`). Adding that handoff commit on top of the code commit moves the
branch tip to a new SHA, so:

- CI re-runs on the new tip. Cite the **tip** SHA (the handoff commit) in the PR body `CI` and
  `E2E` lines, not the pre-handoff code commit — otherwise the reviewer/merger verify a stale head.
- Wait for CI on the final tip before finalizing the PR body. In this repo the gate is two jobs,
  `ci`/check and `fail-on-demand`/gate; both must be `success` at the tip.
- A handoff's own `headSha` field can only point at a commit that already exists, so it will name
  the code commit or a prior tip, never the handoff commit that carries it. The PR body, which the
  reviewer and merger actually read, is the authoritative head record — keep it at the true tip.

## `handoff_complete` requires a clean working copy

`legion handoff_complete` refuses while `.legion/<phase>.json` has uncommitted working-copy
changes. After `handoff_write` (which writes the file into the working-copy commit `@`), run
`jj -R "$LEGION_WORKSPACE" new` so the handoff becomes a committed ancestor and `@` is empty/clean.
Only then does `handoff_complete` succeed. Equivalently, `jj split` the handoff file into its own
commit and leave `@` clean.

## Never write scratch files into the workspace root

`jj` auto-snapshots the working copy, so a PR-body file written to the workspace root is captured
into the current commit `@` and silently pollutes it (e.g. the pushed implementation commit). Write
scratch files (PR body, notes) to `/tmp`, or delete them immediately and verify `jj diff` shows
only the intended change before committing/pushing.

## `.legion/*.json` ledgers ride the branch until the pre-merge deletion

Every phase's handoff JSON is committed to the issue branch and therefore appears as an ADDED file
in the PR diff. This is expected: the `.legion/` tree is deleted once, at the reviewer's direction,
just before merge. Because of this, the `E2E` proofs must explicitly carve `.legion/*` out as
non-product ("the only other paths are `.legion/...`, removed before merge") so the
"only `<target-file>` among product files" assertion remains true while the ledgers are still
present.

## Review corrections may arrive as review-level CHANGES_REQUESTED, not inline threads

When a reviewer requests changes at the review level (no inline comment thread),
`legion threads resolve --pr <n> --repo <owner>/<repo>` prints `no unresolved threads` and exits 0,
and a GraphQL `reviewThreads` query returns an empty list. The AC "resolve the review thread"
condition is then **vacuously satisfied** — there is nothing to resolve. Still run
`legion threads resolve` on every corrective push and paste its output into the PR body `Threads`
section, so the record shows the condition was checked, not skipped.

## Thermo pass scope

`ce-simplify-code` and the thermonuclear review pair apply only when the diff touches runtime code.
A docs-only or plain-text-only diff (like appending to a marker file) runs neither; record the
`Thermo` line as omitted with the reason rather than leaving it blank.
