---
title: "Size the PR body by the paths that merge, not by GitHub's running file count"
category: legion
tags:
  - pr-body
  - handoffs
  - size-line
  - review-findings
date: 2026-10-23
status: active
module: legion-worker/pr-body
related_issues:
  - "LEGSMOKE-1015"
  - "sjawhar/legion-smoke#749"
---

# Size the PR body by the paths that merge, not by GitHub's running file count

- Write the `Size` line from the pull request's diff minus `.legion/<KEY>/`: the files that will
  reach the base. Say in one clause that the handoffs are excluded; never count them.
- Expect GitHub's `changedFiles` / `additions` for the pull request to grow by one handoff file on
  every tester and reviewer push while the line stays yours alone to edit. A `Size` that counted
  handoffs is stale the moment the tester pushes, and the reviewer records the mismatch.
- At retro, once the removal commit is on the head, GitHub's count equals the merging set again
  (the change plus at most one `docs/solutions/` note). Refresh the line then, before the retro
  push, so the merger quotes a body that matches the head READY names.

## Evidence

PR sjawhar/legion-smoke#749 (LEGSMOKE-1015). The implementer wrote `Size: 3 files changed,
+147/−0` at its handoff head `48a440a3`, counting `smoke/LEGSMOKE-1015.md` plus
`.legion/LEGSMOKE-1015/plan.json` and `implement.json`. The tester's push added `test.json`;
at `d74fc8ea` GitHub reported 4 files, +209/−0, and the reviewer logged a minor finding and a
`Not proven / risk` bullet for the stale line (`review.json` `keyFindings[0]`). The set that
merged was the one marker plus this note. Counting only the merging paths from the start would
have left the line true at every head.
