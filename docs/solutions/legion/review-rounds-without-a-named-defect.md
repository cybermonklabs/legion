---
title: "Handling Legion review rounds that request changes but name no defect"
category: legion
tags:
  - code-review
  - review-rounds
  - single-file-scope
  - implementer
date: 2026-09-23
status: active
module: legion-workflow
related_issues:
  - "S391665786-1"
  - "sjawhar/legion-smoke#103"
---

# Handling Legion review rounds that request changes but name no defect

## Context

A one-file change (append a single proof line to `README.md`) went through the full Legion
lifecycle on `sjawhar/legion-smoke` (PR #103). The reviewer returned `CHANGES_REQUESTED`
three times, each with only a generic body (`Stage 3 proof scripted changes requested,
round N`) and **no inline review thread and no named defect**, then approved the third
corrected head. This is the situation this document is about: a change request with nothing
concrete to act on.

## Learnings

- **A change request with no thread and no named defect is still actionable, but only as an
  honest, defensible, in-scope refinement — never a fabricated defect.** Pick the single most
  defensible improvement the current diff genuinely admits, make it, and state plainly in the
  PR body and the handoff that the review named no specific defect and that the correction is a
  best-effort refinement. Do not invent a defect narrative to justify the edit, and do not
  reword just to have "made a change."

- **When there is no thread, record the disposition anyway.** `legion threads resolve` prints
  `no unresolved threads`; the PR body's `Threads` section should say the reviews opened no
  threads and list each correction against the commit that made it, so the reviewer and merger
  can trace what each round changed without an inline anchor.

- **A single-file scope survives multi-round review only if every correction is mechanically
  isolated to that one file and re-verified through the real surface each round — not eyeballed.**
  After every corrective push, read the file back through the user-facing surface
  (`gh api repos/<o>/<r>/contents/<path>?ref=<branch>`) and re-confirm the product diff excludes
  process artifacts (`jj diff --from main@origin --to <branch> --git '~.legion'`). Tooling proof,
  every round.

- **Rewording an already-approved-spec line across rounds is a drift smell; keep it inside the
  acceptance criteria.** The proof line changed three times
  (`- …landed via Legion (issue …)` → drop bullet → `verified` → `(Dispatch issue …)`). This is
  safe only because the acceptance criteria pinned *that a line records the proof* and *that only
  `README.md` changes* — never the exact wording. If a review keeps asking to reword a line the
  spec fixed verbatim, that is a signal to ask for explicit guidance or to update the spec, not to
  keep guessing; unbounded rewording is scope creep and reviewer fatigue.

- **The implementer is the only role that can push, so its corrective push carries the
  review-App phases' handoff commits.** The tester and reviewer run as the review App
  (`legion-reviewer[bot]`), which has no `contents` permission; their `test: record handoff`
  commits sit on the branch locally and ride the implementer's next `jj git push`. Advance the
  existing bookmark to the new tip and push the whole chain; never create a replacement bookmark.

## Deliberately NOT recorded

Generic advice like "communicate with the reviewer" or "write good commit messages" is omitted:
it is context-free and would not help a future worker facing a no-defect change request.
