---
title: "Prove a docs-only change by its bytes at an explicit SHA"
category: legion
tags:
  - proof
  - contents-api
  - negative-control
  - check-runs
  - first-push
date: 2026-10-23
status: active
module: smoke-proof
related_issues:
  - "LEGSMOKE-1021"
  - "sjawhar/legion-smoke#757"
---

# Prove a docs-only change by its bytes at an explicit SHA

A change with no runtime (one Markdown file) still needs a proof on a production-like surface. For
a file in a GitHub repository that surface is the repository itself, read through the API at a
commit SHA. These rules keep such a proof exact and keep its controls from damaging the branch.

- Read the file at an explicit commit, never from the shared workspace checkout and never at a
  branch name:
  `gh api 'repos/<o>/<r>/contents/<path>?ref=<sha>' -H 'Accept: application/vnd.github.raw'`.
  Whichever phase is active mutates the checkout, and a branch name moves under a later push.
- A criterion that says "on the branch's first push" binds to the first push that carries the
  file, not to the branch's literal first push: on a Legion branch the planner's handoff push
  (`.legion/<issue>/plan.json`) comes first and carries no change. Record that SHA once
  (`firstCodeHead` in `implement.json`) and read every later "unchanged" comparison at it.
- Pair every count with bytes. `wc -l` counts newline characters, so a one-line check is the set
  `wc -l` = 1, `tail -c1 | od -An -tx1` = `0a`, `wc -c` = the recorded byte count, and `sha256sum`
  = the hash recorded in the plan from the intended text; `head -1` is compared to the recorded
  line as a string. Use `od`, not `xxd`: the worker image has no `xxd`.
- Negative controls are read-only. The same read at `?ref=<default branch>` and at the fork point
  must answer 404 (`docs/solutions/legion/measure-the-issue-branch-from-its-fork-point-LEGSMOKE-1016.md`
  says why the fork point, not the base tip); a comparison against a deliberately altered string
  must mismatch. For a required check whose workflow fails on a probe file, the control is
  `gh pr view <n> --json mergeStateStatus` printing `BLOCKED` at a trailered handoff head that has
  zero check-runs, never the probe file: a repository with no root `.gitignore` snapshots the probe
  into the next commit, the required check goes red for every tree on the base, and the
  changed-files criterion fails.
- Each push to a branch with an open pull request starts a `push` run and a `pull_request` run of
  every workflow at the same SHA, so `commits/<sha>/check-runs` lists two entries per job. Cite the
  `pull_request` pair in the CI line (the merge gate's CI-head read selects it) and name the push
  pair beside it, so a reader does not take four entries for two jobs as a duplicate workflow.

## Evidence

Pull request sjawhar/legion-smoke#757, branch `legion/LEGSMOKE-1021`, 2026-10-23:

- The branch's first push was the planner's `plan: record handoff` (320860fb); the first push
  carrying `smoke/LEGSMOKE-1021.md` was 86f61ca3, recorded as `firstCodeHead`. The implementer,
  tester and reviewer read the one-line criterion at that SHA; the tester also read it at the
  handoff head 8dec9cbb through the JSON `.content` route and found the same sha256
  `daa7e5ccadb0322b5a8c0aa9bf04fe74a512b0f9b8ea55e8905089dd71d4c10c` (93 bytes, 1 line, last byte
  `0a`, no CR, no BOM).
- Controls: `?ref=main` and `?ref=a50a23aa` (fork point) → `HTTP/2.0 404 Not Found`; the altered
  string `LEGSMOKE-1021x: …` → mismatch; at the trailered handoff head 8dec9cbb, check-runs
  `total_count` 0 and `mergeStateStatus` `BLOCKED` while `mergeable` was `MERGEABLE`. No `.fail-me`
  was created in any phase.
- At 86f61ca3 the check-runs listed `gate` and `check` twice each: the `pull_request` runs
  39900808307 (`fail-on-demand`) and 39900807885 (`ci`), and the `push` runs 39900726551 and
  39900726587, all `completed`/`success`. The CI line cites the first pair and names the second.
