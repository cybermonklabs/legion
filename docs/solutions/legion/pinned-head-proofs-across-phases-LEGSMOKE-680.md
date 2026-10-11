---
title: "Pinned-head proofs when a pull request's file list and file bytes change across Legion phases"
category: legion
tags:
  - legion-proof
  - pinned-sha
  - review-round
  - ci-red-fixture
  - gh-api
date: 2026-10-11
status: active
module: legion-smoke/smoke
related_issues:
  - "LEGSMOKE-680"
  - "sjawhar/legion-smoke#557"
---

# Pinned-head proofs when a pull request's file list and file bytes change across Legion phases

Every Legion phase pushes a handoff commit, a review round can change the bytes under proof, and
an outside push can add a path the acceptance forbids. A proof that reads "the PR" or "the file"
without a SHA is true for one push and stale at the next. These rules keep every role's record
comparable.

## Rules

- Derive the expected shape of a review-driven file from the review state **at the judged
  head's own commit time**: count only requests whose `created_at` precedes that commit's
  `committer.date`, re-read the date for every SHA you judge, and never carry a count from an
  earlier head. State the count (`K=…`) and the request's comment id in `observed`.
- Pin item-level evidence to a full 40-character SHA with the contents API (`?ref=<sha>`) and the
  compare API (`compare/<base>...<sha>`). Treat `gh pr diff --name-only` and `gh pr view --json
  files` as live reads: record the head they were taken at, and expect them to gain a row at every
  handoff push. Write the two expected file sets down before the first proof — before retro
  (target paths plus `.legion/<KEY>/`) and at the head that merges (target paths plus
  `docs/solutions/` only) — and filter by path prefix against the pinned compare.
- When an acceptance line names an event that already happened without the change (here "the
  branch's first push", which the daemon made at the base), record a plan-reviewed spec departure
  that pins the SHA every later role judges instead (`firstFilePush`); do not reinterpret it
  silently in one role's handoff.
- A CI-red fixture pushed onto the branch by a non-Legion account (`.fail-me` for
  `fail-on-demand`) is cured by one forward, untrailered commit that deletes it, on the architect's
  or the driver's word. Never rewrite, revert-merge or rebase it away: the base-pinned compare nets
  the add/delete pair out of the pull request's files and the squash merge keeps it off `main`.
  Record the fixture head, its red run ids and the curing head in the handoff's `rounds[]`.
- Give every proof entry a negative control that would fail if the positive read were wrong for
  the reason you most expect: the other arm's bytes under `cmp`, the same contents call at the
  base (`404`), a wrong path (`404`), a short SHA's run list (`[]`, never "green"), and a trailered
  handoff head's run list (`[]`, judged only through its nearest code ancestor J).

## Evidence

- `sjawhar/legion-smoke#557` (LEGSMOKE-680): `smoke/LEGSMOKE-680.md` was 56 bytes / 1 line at
  `9b87a083` (`firstFilePush`) and 95 bytes / 2 lines at `b4c6fed7` after review 5484970211
  asked for line 2; the K predicate (`request.created_at 21:13:09Z < committer.date 21:15:06Z`)
  selected the two-line shape at `b4c6fed7` and the one-line shape at every earlier head, and the
  tester reproduced it at a later handoff-only head with a different committer date.
- `gh pr diff 557 --name-only` listed 2 paths at `9b87a083`, 3 at `9380966`, 5 at `b4c6fed7`;
  the pinned `compare/a6d5dc7d...<sha>` plus the prefix filter printed exactly
  `added smoke/LEGSMOKE-680.md` at each of them (implement.json item 4; the reviewer's round-1 minor
  asked for the live-read head to be recorded, which round 2 did).
- The proof driver pushed `1eb1440d` (`added .fail-me`, `gate` failure runs 38173307180 and
  38173310089); `da28dc30` (`fix: remove the .fail-me fixture`, untrailered) turned all four runs
  green, and `compare/a6d5dc7d...da28dc30` carried no `.fail-me` row while
  `compare/a6d5dc7d...1eb1440d` did.
- The plan's spec departure (plan.json `specDepartures[0]`) pinned item 2 to `firstFilePush`
  because `legion/LEGSMOKE-680` already existed at the base `a6d5dc7d` without the file.
