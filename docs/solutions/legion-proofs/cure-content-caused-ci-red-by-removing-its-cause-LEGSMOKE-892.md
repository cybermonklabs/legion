---
title: "Cure a content-caused CI red by removing its cause in a new commit, never by rerunning"
category: legion-proofs
tags:
  - ci
  - red-check
  - rerun
  - issue-branch
date: 2026-10-21
status: active
module: .github/workflows
related_issues:
  - "LEGSMOKE-892"
  - "sjawhar/legion-smoke#712"
---

# Cure a content-caused CI red by removing its cause in a new commit, never by rerunning

A check that fails because of what the tree contains is cured only by a new tree.

- Read first: the red head's commit (author, files), the failing job's condition, and whether
  the offending path exists at the red head and on the base
  (`gh api "repos/<owner>/<repo>/contents/<path>?ref=<sha>"`). A path present at the red head and
  absent on the base is the branch's to cure; one present on the base is reported to the
  architect, since the branch cannot cure it without a change outside its scope.
- Build on the moved remote tip (`jj git fetch && jj new legion/<KEY>@origin`), remove the cause
  in a deletion-only code commit (no `skip-checks` trailer, no bracket keyword), push, and read
  the new head's check-runs. Never `gh run rerun` (it re-runs the same tree and stays red or
  greens a SHA whose tree still holds the cause), never force-push.
- Record the red head's run ids and conclusions beside the cure head's in the PR body's `CI`
  line, with the negative control that attributes the red to the cause: the offending path's
  `.size` at the red head beside the failing job there, and a 404 for it at the cure head.
- A path added and deleted on the branch nets out of `gh pr diff --name-only` and
  `pulls/<n>/files`, so a changed-files criterion stays satisfied; check it at the cure head
  rather than assuming it.

## Evidence

- `sjawhar/legion-smoke#712` (LEGSMOKE-892): commit `beb7bfb1` (`sjawhar-agent[bot]`) added
  `.fail-me`; the `fail-on-demand` job `gate` failed on both its `push` and `pull_request` runs
  (39526051626, 39526057184) while `ci`'s `check` stayed green. The deletion-only commit
  `18005c59` (`smoke: remove .fail-me so the gate check passes on legion/LEGSMOKE-892`) produced
  four green check-runs; `.fail-me` → size 25 at `beb7bfb1`, 404 at `18005c59` and on `main`;
  `.fail-me` absent from the pull request's file list afterwards.
