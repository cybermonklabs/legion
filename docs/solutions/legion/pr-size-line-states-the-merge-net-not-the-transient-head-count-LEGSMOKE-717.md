---
title: "The PR body's Size line states the merge net, not the transient head count"
category: legion
tags:
  - pr-body
  - size-line
  - handoffs
  - squash-merge
date: 2026-10-13
status: active
module: smoke
related_issues:
  - "LEGSMOKE-717"
  - "sjawhar/legion-smoke#563"
---

# The PR body's Size line states the merge net, not the transient head count

Write `Size:` as what the squash commit will carry: the changed paths a user of the repository
meets, with their added and removed line counts, and name the `.legion/<issue>/` handoffs as
excluded because retro removes them. Do not quote `gh pr view --json additions` at a head: on a
Legion branch every tester and reviewer round adds or grows a handoff file, so that number is
stale minutes after it is written and stale again after retro.

Recompute the line at every push you make, and once more in retro when the handoffs leave and
`docs/solutions/` arrives. The merger reads `Size` beside its own
`jj diff --from <approved-sha> --to <tip-sha> --summary`; a number true of an earlier head reads as
a wrong claim, not as history.

## Evidence

- Round 1 of PR #563 read `Size: 1 file changed, +1/−0` while GitHub listed four added files
  (`+233/−0`): the reviewer recorded it as a fast-follow accuracy finding.
- Round 3 replaced it with `5 files, +297/−0`, the `gh pr view --json additions` value at code head
  `515a1dd3`. By the approved head `c28a4e2d` the same PR listed `+362`, because the implement,
  test and review handoffs had each been rewritten larger. The retro's fresh-eyes reader found the
  mismatch inside one body.
- The merge-relevant net never changed across those heads: `smoke/LEGSMOKE-717.md` +2 lines, plus
  whatever `docs/solutions/` retro adds.
