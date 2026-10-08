---
title: "A CI-red takeover cancels the tester mid-command and leaves the implementer's working copy behind the remote"
category: legion
tags:
  - ci-red
  - takeover
  - legion-tester
  - legion-implementer
  - jj
date: 2026-10-08
status: active
module: legion-phase-workflow
related_issues:
  - "LEGSMOKE-526"
  - "sjawhar/legion-smoke#525"
---

# A CI-red takeover cancels the tester mid-command and leaves the implementer's working copy behind the remote

When CI settles red on the pull request's head while the tester (or a reviewer on an
uncompleted round) is working, the daemon moves the issue back to `implementing` and interrupts
that role's turn: its running tool call is cancelled and nothing it had in flight resumes. Keep a
tester's verification steps short and re-derivable from GitHub state at a named SHA; never park
evidence in a long-running local process that must survive to completion.

The implementer that is started by the takeover finds its working copy where it left it, on its own
last handoff, while `legion/<KEY>@origin` carries the commit an outside party pushed. Before any
edit: `jj -R "$LEGION_WORKSPACE" git fetch`, read the failing step, then
`jj -R "$LEGION_WORKSPACE" new legion/<KEY>@origin` and cure by cause as
docs/solutions/legion/ci-red-cure-by-cause-LEGSMOKE-504.md says. The outside commit keeps its
author; it is not rewritten, and the cure commit sits above it.

## Evidence

- test.json `notes[2]`: the daemon cancelled the tester's in-flight command
  `/usr/bin/sleep 1207 && date -u … > /tmp/stage4b-takeover-witness && echo WITNESS-WRITTEN` when
  CI settled red at `e8bb497d`; the witness file was never written while the control file written
  beforehand holds `2026-10-08T14:22:22Z`; the tester's next round started from the daemon's new
  task line, not from the interrupted work.
- The implementer's status at the takeover task showed `@-` = `23667df8` (its round-1 handoff)
  while `jj git fetch` moved `legion/LEGSMOKE-526@origin` to `e8bb497d` (`Stage 4b proof:
  fail-on-demand`, author `sjawhar-agent[bot]`, diff exactly `A .fail-me`). The cure `8c769387`
  was made on `jj new legion/LEGSMOKE-526@origin` with diff exactly `D .fail-me`; `legion push`
  moved the bookmark forward `e8bb497d → 8c769387`.
- Both `fail-on-demand` rows failed at `e8bb497d` (runs 37792447133, 37792454720) and all four rows
  passed at `8c769387`; `contents/.fail-me?ref=8c769387` → HTTP 404.
