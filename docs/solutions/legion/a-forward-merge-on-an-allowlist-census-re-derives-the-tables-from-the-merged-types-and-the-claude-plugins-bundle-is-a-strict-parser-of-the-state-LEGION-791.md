---
title: "A forward merge on an allowlist census re-derives the tables from the merged types, and the Claude plugin's committed bundle is a strict parser of the state, not a copy of the number"
category: legion
tags:
  - daemon-api-contract
  - forward-merge
  - allowlist
  - claude-envoy
  - check-dist
  - contract-change-census
date: 2026-10-21
status: superseded in part by sjawhar/legion#2423 — a contract change no longer budgets a bundle rebuild, and a pull request runs no `check-dist`; main's Rebuild Claude Envoy plugin workflow commits the bundle after each merge; and by dispatch://INFRA-1569 — a contract change adds an entry with scripts/daemon-contract-entry.sh and no file declares its number, so the plan's number and the four declarers went; that the bundle is a strict parser of the state and the re-census at a forward merge stand
module: packages/daemon/internal/api
related_issues:
  - "LEGION-791"
  - "sjawhar/legion#2096"
---

# A forward merge on an allowlist census re-derives the tables from the merged types, and the Claude plugin's committed bundle is a strict parser of the state

Extends docs/solutions/legion/a-long-branch-loses-the-serial-number-it-reserved-INFRA-31.md.

That note gives the forward-merge discipline. The delta:

- A forward merge on a census-shaped change re-opens the census. The base
  can add configuration fields the allowlist must classify and remove fields the plan's table
  named: re-run the classification over the merged types, decide each new field (emit or exclude
  with a reason), and drop the rows whose fields are gone. A test that derives its type set by
  walking the root type — rather than a hand list — makes this re-census the test's own failure
  message instead of a reading exercise.
- `packages/claude-envoy`'s committed `dist/` bundle is a fourth strict parser of the state
  document: a new required member of `LegionStateResponse` migrates inside the bundle, and only
  `bun run build` on the pinned Bun moves it; lint and typecheck cannot see a stale bundle. Until
  dispatch://INFRA-1569 a contract change also wrote its number into four declarers
  (`version.go`, `fixtures/daemon-api/version.json`, `pi-legion/package.json`,
  `claude-envoy/package.json`); it adds an entry since, and no file declares its number.
- Re-run the same live predicate set at the pre-merge head, the merge head and the fix head, so the
  record shows the merge changed no behaviour: the fingerprint comment says the diff changed, the
  three identical runs say what did not.

## Evidence

sjawhar/legion#2096 (LEGION-791) was planned at contract 30; while the plan was written the base
took 30 through 34. GitHub reported the first push `CONFLICTING` (17 files); one forward merge
resolved it at 35 with both histories kept in `version.go`'s entry. The merge removed
`config.Project.Repo` (the plan's `projects.<KEY>.repo` row; the live proof's negative control was
the loader's refusal, `Unknown key "projects.FIXTURE.repo"`) and added `production_check_hours`,
`worker_backends`, `runtime.kubernetes.setup` and `runtime.kubernetes.repository_cache`, which the
derived type walk reported as unclassified until each had a row. The simplify pass's quality
reviewer, not CI, found `packages/claude-envoy/package.json` still at 34 with a bundle inlining the
pre-change strict schema; `bun run build` and `check-dist` closed it. The tmux real-binary proof
ran unchanged at 818b9654, defed47c (the merge) and 9b1a38ef.
