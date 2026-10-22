---
title: "Bound jj revsets to the issue branch in the shared clone"
category: legion
tags:
  - jj
  - revset
  - shared-clone
  - legion-workspace
date: 2026-10-22
status: active
module: issue-workspace
related_issues:
  - "LEGSMOKE-1009"
  - "sjawhar/legion-smoke#746"
---

# Bound jj revsets to the issue branch in the shared clone

Every Legion issue workspace of this repository is a `jj workspace` of one shared clone, so the
clone's commit graph holds every issue's branch, past and present. A revset that is not anchored
to your own branch reads all of them.

- Anchor every log, diff, or file listing to the issue branch or the working copy:
  `main@origin..legion/<KEY>@origin`, `ancestors(@, N)`, or `legion/<KEY>@origin`.
- Never use `all()`, `all() ~ ancestors(main@origin)`, or a description or author filter with no
  branch anchor to "see what is unpushed": the result is other issues' commits, and a path or
  commit you act on from it is not yours.
- The identity check `"${default}@origin..@"` from `skill://legion-worker/references/default-branch-check.md`
  is already bounded; keep it that way, and check `jj workspace list` when a log looks too long.

## Evidence

On 2026-10-22, before the first push of pull request sjawhar/legion-smoke#746, the implementer ran
`jj log -r 'all() ~ ancestors(main@origin)'` to list its unpushed commits. It printed 388 commits
from every earlier smoke issue's branch (`plan: record handoff`, `retro: remove .legion/…`,
`smoke: add LEGSMOKE-1007 marker file`, …), where the bounded
`main@origin..legion/LEGSMOKE-1009@origin` printed 5. `jj workspace list` showed only 2 workspaces
on the clone at the time: the commits of earlier issues' branches stay in the clone's graph after
their workspaces are gone.
