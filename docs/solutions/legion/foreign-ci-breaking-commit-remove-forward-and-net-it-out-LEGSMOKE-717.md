---
title: "A foreign CI-breaking commit on the issue branch: remove it forward and net it out"
category: legion
tags:
  - ci-red
  - foreign-commit
  - forward-commit
  - pre-push-guard
date: 2026-10-13
status: active
module: smoke
related_issues:
  - "LEGSMOKE-717"
  - "sjawhar/legion-smoke#563"
---

# A foreign CI-breaking commit on the issue branch: remove it forward and net it out

When a commit nobody in the Legion chain authored lands on `legion/<issue>` and turns CI red,
start a fresh change on `legion/<issue>@origin` and delete its payload in one forward commit that
touches nothing else. Never rebase it away or move the bookmark backwards: the daemon returned the
issue to implementing because the head is red, and a rewrite of a pushed commit is the one move
that needs the rewrite guard.

Read a GitHub-API commit's identity for what it is: author the acting App or user, committer
`noreply@github.com`. It is not a sign that something rewrote your commits; the identity check
that matters is still "my role's App in both columns on every commit I made".

Before the push, confirm the removal nets the foreign payload out of the pull request:
`jj diff --from 'main@origin' --to @- --name-only` must list only the paths the issue allows, and
after the push `gh pr diff <n> --name-only` must agree. The foreign path then appears in no
changed-files moment the spec caps.

## Evidence

- `002f2f42c956f95ffff0970721ecba2b1be17322` (`Stage 4b proof: fail-on-demand`, author
  `sjawhar-agent[bot]`, committer `noreply@github.com`) added `.fail-me`; `gate` failed on both the
  push and pull_request runs, and the daemon reopened implementing with
  "CI is red at 002f2f42…: gate".
- `30260cab1867e602a8f69148c3dafdaf2d3ce939` (`smoke: remove .fail-me so fail-on-demand passes`)
  is a forward commit above it whose own diff is the one deletion. `jj diff --from main@origin`
  listed only `smoke/LEGSMOKE-717.md` and `.legion/LEGSMOKE-717/*`; both workflows were green at
  `30260cab` (gate run 38347616009, check run 38347616004); `gh pr diff 563 --name-only` showed no
  `.fail-me`, and the negative control at `002f2f42` still showed `gate` failure.
