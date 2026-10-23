---
title: "Size the pull request body by the paths that merge"
category: legion
tags:
  - pr-body
  - size-line
  - handoffs
  - retro
date: 2026-10-23
status: active
module: smoke-proof
related_issues:
  - "LEGSMOKE-1022"
  - "sjawhar/legion-smoke#758"
---

# Size the pull request body by the paths that merge

On a Legion branch the pull request's diff grows by one `.legion/<issue>/*.json` file per phase
and shrinks again when retro removes the directory, so a `Size:` line that counts the PR diff at
the head it was written at is stale at every later head and draws a tester observation and a
reviewer finding each run.

- Write `Size:` as the diff that reaches the base: the paths outside `.legion/<issue>/`, with
  their `+added/−removed`, and say so in the line (`merge diff; handoffs under .legion/<issue>/
  excluded, removed by retro`). Read it from GitHub at the first code head:
  `gh api 'repos/<o>/<r>/compare/<fork-point>...<firstCodeHead>' --jq '[.files[] | select(.filename | startswith(".legion/") | not) | {filename, additions, deletions}]'`.
- A tester or reviewer compares the line to that filtered set, not to `gh pr view --json changedFiles`,
  whose count includes the handoffs.
- Retro re-reads the line at its removal head, where the filtered set is the whole diff
  (`jj diff --from 'fork_point(<base>@origin | @-)' --to @- --stat`), and adds the
  `docs/solutions/` note it wrote; a stale `Size:` is one of the lines retro's own commits make
  stale, so it is refreshed before the retro push, never left for a fast-follow.

## Evidence

Pull request sjawhar/legion-smoke#758, branch `legion/LEGSMOKE-1022`, 2026-10-23:

- The implementer wrote `Size: 2 files changed in the PR diff, +115/−0` at the first code head
  89fedc99 (`smoke/LEGSMOKE-1022.md` +1 and `.legion/LEGSMOKE-1022/plan.json` +114), noting that
  retro would remove the handoff. At the tester's head 75ed1828 the PR diff was 3 files, +192; at the
  reviewed head 0941b276, 4 files; at the approved head ab1f55b3, 5 files, +320/−0. The tester
  recorded the drift in `test.json` `observations` and the reviewer as the round's one `minor`
  finding; neither was a defect in the change, and both cost a read.
- The number that was stable throughout was the filtered one: `smoke/LEGSMOKE-1022.md` +1/−0, the
  only path outside `.legion/LEGSMOKE-1022/` until this retro added its note.
