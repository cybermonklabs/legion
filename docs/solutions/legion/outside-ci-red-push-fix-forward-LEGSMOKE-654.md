---
title: "An outside CI-breaking push on the issue branch: fix forward, and prove it from the fixture head"
category: legion
tags:
  - ci-red
  - fail-on-demand
  - outside-push
  - compare-api
  - negative-control
  - run-list-scoping
date: 2026-10-11
status: active
module: legion-smoke/fail-on-demand
related_issues:
  - "LEGSMOKE-654"
  - "sjawhar/legion-smoke#552"
---

# An outside CI-breaking push on the issue branch: fix forward, and prove it from the fixture head

When a commit nobody in the tree made lands on `legion/<KEY>` and turns a required check red:

- Diagnose at the red head before anything else: read the breaking file at that SHA
  (`gh api "repos/$R/contents/.fail-me?ref=$S" -i | sed -n 1p`) and the commit's author
  (`gh api "repos/$R/commits/$S" --jq '{author: .author.login, files: [.files[].filename]}'`).
  HTTP 404 means the job failed on its own: `gh run rerun <run-id> --failed` once, push nothing.
  HTTP 200 with an author that is not a Legion App means an outside push: no rerun cures it.
- Fix forward. Start a new change on the remote tip (`jj new legion/<KEY>@origin`), delete the
  file and nothing else, commit it as a plain code push. Never rewrite, revert or abandon the
  outside commit: another role may already stand on that tip, and the commit is the audit trail.
- Prove from the fixture head, not the fixed head. GitHub's compare API from the base to the new
  head carries no row at all for a file that was added and then deleted (not even a `removed`
  row), so a clean "only this file changed" list at the fixed head cannot tell whether the
  fixture ever existed. The negative control is the fixture head's own compare (`added .fail-me`)
  and its run list (`gate` `completed/failure`); record both beside the clean list.
- Scope every run-list read by branch:
  `gh run list -R $R --commit $S --json … --jq '.[] | select(.headBranch=="legion/<KEY>")'`.
  A base SHA shared by many bookmarks carries dozens of sibling runs, and the branch's own
  creation push leaves runs for the branch at the base; an unscoped list reads green or red from
  the wrong branch.
- Name the merge mechanics in the body. The fixture commit stays reachable from the pull request's
  head; only a squash merge keeps that tree out of `main`'s reachable history. Put the sentence in
  `Not proven / risk` so the merger quotes it in READY and submits `--squash`.

## Evidence

Pull request sjawhar/legion-smoke#552 (LEGSMOKE-654). The implementer's handoff head
`293b13b964991c0306afe1b92f34890019ee214f` was followed by `1360615c280bd41348ceb98a2f518beeec1148c4`
("Stage 4b proof: fail-on-demand", author `sjawhar-agent[bot]`), which added a 25-byte `.fail-me`.
At that head `contents/.fail-me` answered HTTP 200 and the scoped run list read
`fail-on-demand push completed/failure 38099760252`, `fail-on-demand pull_request completed/failure 38099764220`,
with `ci` 38099760279 / 38099764184 success; the pull request showed `mergeStateStatus BLOCKED`.

The fix was one forward commit `bc693ea9407bcf3c9c5cef14855c7257ecd40b60` ("ci: remove the .fail-me
fixture …", `jj diff --summary` = `D .fail-me`). At that head all four runs were `completed/success`
(`ci` 38099908290 / 38099910039, `fail-on-demand` 38099908298 / 38099910038), `.fail-me` answered 404
and the pull request was `CLEAN`. `compare/68e4d897…...bc693ea9…` listed only the two handoffs and
`added smoke/LEGSMOKE-654.md` — no `.fail-me` row in either direction — while
`compare/68e4d897…...1360615c…` listed `added .fail-me`; the tester confirmed the clean tree
independently through `git/trees/<head>?recursive=1` (no `.fail-me` blob). The unscoped run list at
the base `68e4d897…` held 68 runs; the `headBranch` filter kept 2 (the branch's creation push).
