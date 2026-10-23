---
title: "Judge a pre-seeded issue branch from its pre-Legion tip, and what merges from the fork point"
category: legion
tags:
  - fork-point
  - fixture-commit
  - two-bases
  - plan-pre-state
  - spec-departure
  - push-guard
date: 2026-10-23
status: active
module: issue-workspace
related_issues:
  - "LEGSMOKE-1056"
  - "sjawhar/legion-smoke#763"
---

# Judge a pre-seeded issue branch from its pre-Legion tip, and what merges from the fork point

Extends docs/solutions/legion/measure-the-issue-branch-from-its-fork-point-LEGSMOKE-1016.md.
That note measures the branch from `fork_point(main@origin | @-)`. When a commit nobody in the
tree authored sits between that fork point and the planner's first commit (a proof driver's
fixture, a human's seed commit), the fork-point diff reports its paths as the branch's own, and a
criterion that says "the diff touches no other path" fails on files Legion never wrote.

- Record two bases in the plan's constants: `B`, the branch tip at planning
  (`legion/<KEY>@origin` before the plan push), and `F`, the fork point. Legion's own change —
  every path guard, the `Size:` line, the "no other path" criterion — is judged from `B`
  (`jj diff --from <B> --to @- --name-only`; `gh api 'repos/<o>/<r>/compare/<B>...<sha>'`). What
  the merge lands on the base is measured from `F` and recorded beside it, never hidden.
- Never rewrite, remove or edit the pre-Legion commit's files to make the fork-point diff clean:
  no role may rewrite a pushed commit it did not author, a later deletion changes what the next
  phase's session loads from the repository, and READY refuses any path above the approved head
  but retro's. Record the landing as a spec departure (`plan.json` `specDepartures`) and send the
  decision to the architect; proceeding is the default.
- Add the pre-Legion commit's invariant to every phase's pre-push guard:
  `compare/<B>...<sha>` → `status: ahead`, and `B` an ancestor of `legion/<KEY>@origin`. Any other
  answer is a reset of the branch: stop, report, and re-use no recorded proof SHA.
- State the "nothing else on the branch" pre-state fact as the path set from `B` with
  `.legion/<KEY>/` excluded — empty before the first code push — never as a count of files from
  the base: every handoff push raises that count by one, so a literal count is stale by the time
  the next role reads it.
- Read the proof's 404 controls at both `F` and `B`: `B` proves the file is new to the branch,
  `F` that it is new to the base.

## Evidence

Pull request sjawhar/legion-smoke#763, branch `legion/LEGSMOKE-1056`, 2026-10-23:

- The proof driver pushed 61ef66de ("Stage 4b fixture…", author `stage4b@legion.invalid`, 25
  added files: `AGENTS.md`, `.omp/*`, `.claude/*`, `.codex/*`, `.mcp.json`, `lsp.json`, Go,
  TypeScript and Python roots) as the branch's only commit above the fork point 2db619cf before
  planning. `compare/main...legion/LEGSMOKE-1056` at planning (13:13Z): `diverged`, `ahead_by 1`,
  25 files all `added`.
- The plan (D-F1) set `B` = 61ef66de and `F` = 2db619cf. At the first code head fcab2e1c,
  `compare/61ef66de...fcab2e1c` listed `.legion/LEGSMOKE-1056/plan.json` and
  `smoke/LEGSMOKE-1056.md`; the `Size:` line read `1 file changed, +1/−0 — Legion's diff from the
  fixture commit 61ef66de; the 25 fixture paths below it and the handoffs excluded`, and the
  tester and reviewer compared to that filtered set while the pull request's own file list showed
  28 files at the tester's head.
- The 404 controls: the raw read of `smoke/LEGSMOKE-1056.md` at `?ref=2db619cf` (F), at
  `?ref=61ef66de` (B) and at `?ref=main` answered HTTP 404 in every phase.
- The count from `main` drifted where the plan expected it not to: D15(6) said "still 25 added
  paths"; the implementer read 26 before its push (the plan handoff had landed) and 27 after the
  marker. The invariant from `B` (`ahead`; files ⊆ the marker and `.legion/LEGSMOKE-1056/`) held
  at every head, so no role stopped.
- The 25 fixture paths land on `main` at the squash. Recorded as Departure 1 in `plan.json`
  `specDepartures`, sent to the architect with the plan's completion and again in the implementer's
  report, and carried into the issue's acceptance: the architect's `sign_off` moved the issue to
  retro with the departure standing. No role touched the fixture's files.
