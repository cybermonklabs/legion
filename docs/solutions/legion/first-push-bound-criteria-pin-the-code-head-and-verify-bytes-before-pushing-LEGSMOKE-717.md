---
title: "First-push-bound criteria: pin the code head and verify bytes before pushing"
category: legion
tags:
  - acceptance-criteria
  - first-push
  - byte-verification
  - handoff-fields
date: 2026-10-13
status: active
module: smoke
related_issues:
  - "LEGSMOKE-717"
  - "sjawhar/legion-smoke#563"
---

# First-push-bound criteria: pin the code head and verify bytes before pushing

A criterion phrased "on the branch's first push" binds a commit that no later push can repair.
Treat it as the implementer's first push of a commit that touches the criterion's path, never the
oldest commit on the branch: on a Legion branch the planner's handoff push comes first and touches
only `.legion/<issue>/`.

Before that push, prove the bytes locally with commands that distinguish a missing final newline
from a missing line: `wc -l`, `tail -c1 <file> | od -An -tx1` (expect ` 0a`), and `od -An -c` for
the whole file. `wc -l` alone cannot tell them apart, and `xxd` is not in the worker image.

Record that commit's SHA in the implement handoff as its own field (`firstPushSha`) and keep
`codeSha` for the newest code head. Every later round rewrites `codeSha`; the first-push SHA must
survive it, because the tester and the production check read the first-push criterion at that
SHA and nothing else.

## Evidence

- Spec criterion 2 of LEGSMOKE-717: "On the branch's first push, that file holds one line ending
  in a newline (`wc -l` reports 1)". The branch `legion/LEGSMOKE-717` existed at `main` before any
  code, and the planner's `plan: record handoff` (`9dafc5c5`) preceded the marker commit.
- Plan decision 1 pinned the first push to `345f8aae80436a7a4c994ec72044e44e65adb499`
  (`smoke: add LEGSMOKE-717 marker file`); `od -An -c` before the push showed
  `... w o r k f l o w \n`, 56 bytes.
- The implement handoff's top-level `codeSha` moved to `30260cab` and then `515a1dd3` across the
  CI-red and review rounds; the reviewer flagged the stale value in round 1. Round 3 added
  `firstPushSha` so criterion 2's anchor no longer depended on which round last rewrote `codeSha`.
