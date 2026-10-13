---
title: "A foreign commit that reddens CI on the issue branch: remove it forward, never rewrite or rerun"
category: legion
tags:
  - ci-red
  - fail-me
  - forward-commit
  - identity-check
  - rerun
date: 2026-10-13
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-705"
  - "sjawhar/legion-smoke#561"
---

# A foreign commit that reddens CI on the issue branch: remove it forward, never rewrite or rerun

- When CI goes red because a commit someone else pushed added a file, start a new change on `legion/<KEY>@origin`, delete only that file, and commit it path-scoped (`jj split -m '<message>' <path>`). No `jj rebase`, no squash, no `--allow-backwards`.
- Do not `gh run rerun --failed` a run that failed because of the injected content: a rerun is for a run red on its own. The removal push starts the runs that matter.
- Push the removal untrailered; it is a code push. Then re-pin the proof at the removal head and cite the red head as the negative control (the fixture present, `gate` failed, the marker's digest unchanged).
- Expect the identity check to show the foreign commit with another author and a `noreply@github.com` committer (a web-API commit) on your chain. That is the fixture, not evidence of a rewrite; your own commits still show your App in both columns.
- Confirm the fixture nets out of the pull request's changed files: `gh pr diff <n> --name-only | grep -c '^<path>$'` → `0` (added-then-deleted paths vanish from `base...head`), and the pre-push guard `jj diff --from main@origin --to @- --name-only` lists only the allowed paths.

## Evidence

- `sjawhar-agent[bot]` pushed e4f8b191b346ff25bd96af4f0e7b7a7c7a956a3c adding a 25-byte `.fail-me`; `gate` failed on both events (runs 38331116746, 38331120485) while `check` passed and `smoke/LEGSMOKE-705.md` kept sha256 `7035913b…13d9`.
- Forward removal 825b17ed6da7c39f253788a3ec01fe34f8bf8f25 (`smoke: remove .fail-me fixture`): four runs green (38331288922, 38331293546, 38331288823, 38331293652), `.fail-me?ref=825b17ed` → 404, `gh pr diff 561 --name-only | grep -c '^\.fail-me$'` → 0; the branch moved forward only. No rerun was issued.
