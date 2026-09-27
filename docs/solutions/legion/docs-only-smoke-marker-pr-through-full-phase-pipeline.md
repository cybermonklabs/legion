---
title: "Driving a docs-only one-file change through the full Legion phase pipeline"
category: legion
tags:
  - docs-only
  - phase-pipeline
  - review-rounds
  - review-threads
  - ci-head-drift
date: 2026-09-27
status: active
module: legion-worker
related_issues:
  - "S392703949-1"
  - "sjawhar/legion-smoke#398"
---

# Driving a docs-only one-file change through the full Legion phase pipeline

A one-file Markdown change (here `smoke/S392703949-1.md`) still runs the whole Legion
pipeline: plan → implement → test → review (repeated) → retro → merge. The following
patterns made it clean and are reusable on any docs-only or marker-file issue.

## Classify docs-only by product surface, not by file count

The PR added five files — four `.legion/*.json` handoffs plus one `smoke/*.md` — yet it is
correctly docs-only, because only `smoke/*.md` is product; `.legion/*.json` are process
artifacts. Record the skip as a positive, reasoned statement in every phase artifact rather
than leaving it implied:

- `implement.json`: `docsOnly: true`, `simplifyRun: "not applicable — docs-only diff …"`.
- `review.json`: `docsOnly: true`, `thermonuclear: "skipped — docs-only PR …"`.

In the PR body, **drop** the `Thermo` line entirely rather than writing "Thermo: skipped";
the READY template omits it on a docs-only PR. `ce-simplify-code` and the thermonuclear pair
do not run when no runtime code changes.

## Pin the "if and only if" append as an acceptance criterion with a negative control

The plan's acceptance criterion 2 — "If and only if the review asks, exactly one more line is
appended and nothing else changes," with negative control "a second line present without a
review request is a violation" — turns vague "address the review" into a byte-checkable
contract. Each review round appends exactly one line, and reviewer/tester prove tightness with
`jj diff -r <correction-sha> --git` showing a single `+` line. This prevents scope accretion
across rounds for free.

## Disposition top-level REQUEST_CHANGES via the corrective push, not `legion threads resolve`

The reviewer's REQUEST_CHANGES rounds carried **no inline comments**, so zero review threads
exist and `legion threads resolve --pr <n> --repo <owner>/<repo>` correctly prints
`no unresolved threads` and resolves nothing. `0 resolved, 0 unresolved` is expected and
sufficient — not a missed step. The disposition record lives in the PR body's `Threads`
section and in `implement.json.threads`, mapping each round to its corrective commit
(round 1 → c8274a37, round 2 → 7a3b1e4b, round 3 → c6428783). Still run `legion threads
resolve` before each corrective push and paste its output, so the "no threads" fact is on record.

## Match the existing file convention exactly, including the bare-key first line

`read` a sibling `smoke/<KEY>.md` (e.g. `smoke/S390606688-1.md`) before writing: its first
line is the bare issue key, and multi-line variants carry one `Stage 3 review round N (<key>)`
line per review round. The file-not-found negative control — `cat smoke/S000000000-0.md` →
`No such file or directory` — proves the check discriminates the specific file rather than
passing for any path; reuse it on every marker-file issue.

## Per-round cadence: a push alone does not finish a review round

Each implementing round is: append the one line → `describe` the correction commit → `jj new`
→ push the issue branch with the single push procedure → wait for CI green → update the PR body
(CI, Threads, E2E lines by SHA) → `handoff_write` the implement handoff (with the required
`proof` array) → `split`/commit and push the handoff → `handoff_complete`. The daemon requires
the `handoff_complete` tool call to end the phase; pushing the fix is not enough.

## Risk: the approved/merged head can sit above the CI- and E2E-verified head

Each phase's handoff commit advances the issue branch, so the head the reviewer approved
(`cd0d386c`, a `review: record handoff` commit) and `reviewedContentHead` (`9cb051e8`) both sit
**above** the head CI last recorded green (`87e99ebb`) and the head the E2E proof named
(`c6428783`). The consequence here is low — the commits above the verified product head touch
only `.legion/*.json` — but as a pattern, the merged SHA is not the CI-proven SHA. A future
worker should confirm CI is green at the actual approval/merge SHA, or keep handoff-state
commits off the reviewed/merged branch so the approved SHA is always the CI-proven SHA.
