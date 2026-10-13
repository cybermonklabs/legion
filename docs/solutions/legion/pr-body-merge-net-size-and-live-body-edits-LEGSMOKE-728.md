---
title: "Keep the PR body's Size as the merge net and edit the live body, never a cached copy"
category: legion
tags:
  - legion-pr-body
  - size-line
  - live-body
  - review-threads
date: 2026-10-13
status: active
module: smoke
related_issues:
  - "LEGSMOKE-728"
  - "sjawhar/legion-smoke#565"
---

# Keep the PR body's Size as the merge net and edit the live body, never a cached copy

Write `Size:` as what the squash merge will land: the code paths with their added and removed
line counts, plus whatever `docs/solutions/` retro adds, and state that the `.legion/<issue>/`
handoffs are excluded because retro removes them. Recompute it at every push and once more in
retro; never quote `gh pr view --json additions` at a handoff head, which counts the handoffs.

From the second round on, other roles have written into the body (the reviewer's `Look at first`
and `Not proven / risk`, the tester's `E2E (tester)`). Before every `PATCH`, read the live body
into a fresh temp file (`gh api repos/<o>/<r>/pulls/<n> --jq .body > "$(mktemp)"`), edit only your
own lines there, and write that file back. A `PATCH` from the copy you wrote at PR open erases
their lines.

Restate the `CI:` paragraph's `Changed files` list at the head you are citing each time a handoff
adds a path; a list one path short is a reviewer finding even when every extra path is allowed.

Run the GitHub-surface proofs before `gh pr create` so the body opens with a true
`E2E (implementer)` line; only the `gh pr diff --name-only` read needs the PR number.

Resolve review threads as the pull request's author, one `resolveReviewThread` call per thread,
only after a disposition reply, and only for your own threads and the bot threads the reviewer
names to you by node id (the review App cannot resolve them). Record each in `Threads:` with its
node id, disposition, reply id and the head it was resolved at.

## Evidence

- `Size:` on #565 read `merge net 1 file, +1/−0` at `41b0cc93`, `+2/−0` after the appended line
  at `c16e9401`, and gained the `docs/solutions/` files in retro; the branch diff at the same
  time listed four `.legion/LEGSMOKE-728/*.json` files that never reach `main`.
- Round-1 review (5501063449) named the `Changed files` list as one path short
  (`.legion/LEGSMOKE-728/test.json` missing at `6ae7108e`); restated at `c16e9401`.
- Round 2's body edit was made from a fresh read of the live body, preserving the reviewer's
  three `Not proven / risk` bullets and the tester's `E2E (tester)` paragraph; thread
  `PRRT_kwDOUCdo886rx_Xx` was replied to (4256671430) and resolved by the implementer, and the
  bot thread `PRRT_kwDOUCdo886ryUWj` was resolved only after the reviewer named it by Envoy.
