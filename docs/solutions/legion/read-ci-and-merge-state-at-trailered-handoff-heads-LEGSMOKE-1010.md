---
title: "Read CI and merge state at trailered handoff heads"
category: legion
tags:
  - skip-checks
  - check-runs
  - merge-state
  - required-checks
  - legion-handoff
date: 2026-10-23
status: active
module: pull-request-gates
related_issues:
  - "LEGSMOKE-1010"
  - "sjawhar/legion-smoke#747"
---

# Read CI and merge state at trailered handoff heads

A Legion handoff-only push ends its commit message with `skip-checks: true`, so GitHub starts no
workflow for it. On this repository that head then has zero check-runs and an empty
`statusCheckRollup`, and the pull request reports `mergeStateStatus: BLOCKED` until retro's
untrailered removal push. None of that is a finding.

- Read CI at the CI head, never at the handoff head you act on. The CI head is the `head_sha` of
  the branch's newest `pull_request`-triggered Actions run
  (`gh api 'repos/<owner>/<repo>/actions/runs?branch=legion/<KEY>&event=pull_request&per_page=30' --jq '.workflow_runs[0].head_sha'`;
  before the pull request exists, the code head's own `push` runs). Read its check-runs there,
  then require `gh api 'repos/<owner>/<repo>/compare/<ciHead>...<head>' --jq '[.files[].filename] | all(startswith(".legion/<KEY>/"))'`
  to print `true`, so the green you cite covers everything but handoffs.
- Treat `mergeStateStatus: BLOCKED` together with `mergeable: MERGEABLE` at a trailered head as
  the expected pre-retro state. Only `mergeable: CONFLICTING` licenses the forward-merge procedure
  in `skill://legion-worker/references/conflicts-and-rewrites.md`. Retro's removal push carries no
  trailer, so the READY head is its own CI head and the merger reads check-runs and the rollup
  there.
- Do not assert the required-check set from one endpoint. The ruleset, classic protection and the
  branch summary answer differently to the role App; record the set as unverified and decide
  READY on `gh pr view <n> -R <owner>/<repo> --json headRefOid,mergeable,mergeStateStatus,statusCheckRollup`
  at the READY head.

## Evidence

Pull request sjawhar/legion-smoke#747, 2026-10-23, branch `legion/LEGSMOKE-1010`:

- Code head `d59db87234ac` (the only untrailered push before retro):
  `commits/d59db872…/check-runs` → `{"names":["check","gate"],"states":["completed/success"]}`;
  `pull_request` runs 39875280559 (`fail-on-demand`/`gate`) and 39875280642 (`ci`/`check`).
- Handoff heads `c5b4b745` (implement), `8fdd018c` (test) and `08951be3` (review, the approved
  head): `check-runs` → `{"total_count":0,"names":[]}` at each; `gh pr view 747 --json
  mergeable,mergeStateStatus,statusCheckRollup` at `08951be3` → `MERGEABLE`, `BLOCKED`, `[]`.
  `compare/d59db872…...08951be3…` listed only `.legion/LEGSMOKE-1010/{implement,test,review}.json`.
- Required-check reads by the role App at the same instant: `rules/branches/main` →
  `required_status_checks: [{context: "gate"}]`, `strict: false`; `branches/main/protection` →
  HTTP 403 "Resource not accessible by integration"; `branches/main --jq '{protected,protection}'`
  → `{"protected":true,"protection":{"enabled":false,"required_status_checks":{"checks":[],"contexts":[],"enforcement_level":"off"}}}`.
