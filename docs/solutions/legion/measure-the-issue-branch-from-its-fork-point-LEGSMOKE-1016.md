---
title: "Measure the issue branch from its fork point, not from the base tip"
category: legion
tags:
  - jj
  - revset
  - fork-point
  - push-guard
  - plan-pre-state
date: 2026-10-23
status: active
module: issue-workspace
related_issues:
  - "LEGSMOKE-1016"
  - "sjawhar/legion-smoke#752"
---

# Measure the issue branch from its fork point, not from the base tip

Extends docs/solutions/legion/bound-jj-revsets-to-the-issue-branch-in-the-shared-clone-LEGSMOKE-1009.md.
That note anchors revsets to the issue branch; this one fixes the other end of the range. Several
trees run against one base at once, so `main@origin` moves between planning and the next push, and
a diff whose `--from` is the base tip shows another tree's merged files as deletions on yours.

- Write a path guard or a changed-files check as
  `jj diff --from 'fork_point(main@origin | @-)' --to @- --name-only`: it lists only the paths this
  branch itself changed, whatever lands on `main` meanwhile. `gh pr diff <n> --name-only` measures
  the same set on GitHub.
- A plan's pre-state names the facts that would invalidate the change (the file absent at
  `ref=main`, zero commits by path on `main`, no open pull request, the required-check set), never
  the base tip's SHA, and its stop condition is "one of those facts changed". A base tip that moved
  is the normal state of a shared base, not a reason to stop.
- Record the fork point once (`jj log -r 'fork_point(main@origin | @-)' -T 'commit_id'`) and read
  the "file is new on this branch" control at that SHA, not at `main`, so the control keeps
  meaning after the base advances.

## Evidence

Pull request sjawhar/legion-smoke#752, branch `legion/LEGSMOKE-1016`, 2026-10-23:

- Planning (02:35Z) recorded `legion/LEGSMOKE-1016@origin == main@origin == eef0146a4838` and a
  guard `jj diff --from main@origin --to @- --name-only` allowing only `smoke/LEGSMOKE-1016.md`,
  `.legion/LEGSMOKE-1016/` and `docs/solutions/`, with "any difference from the planning facts
  stops the implementer".
- At implementing (02:47Z) `main@origin` was `089e712a9922` (tree 1's `smoke: add LEGSMOKE-1015
  marker file (#749)`). The literal guard listed `smoke/LEGSMOKE-1015.md` and
  `docs/solutions/legion/size-the-pr-body-by-the-paths-that-merge-LEGSMOKE-1015.md` beside the
  branch's own paths. `fork_point(main@origin | @-)` was still `eef0146a4838`, and the guard from
  there listed `.legion/LEGSMOKE-1016/plan.json` and `smoke/LEGSMOKE-1016.md` only.
- The four invalidating facts were unchanged (`contents/…?ref=main` 404; `commits?path=…&sha=main`
  0; `gh pr list --head legion/LEGSMOKE-1016` `[]`; required check `gate` only), so the implementer
  proceeded and recorded the base advance in `implement.json` `preState` and `deviations`; the
  retro's fresh-eyes reader flagged the unmet literal stop condition as the one plan-versus-execution
  gap of the run.
