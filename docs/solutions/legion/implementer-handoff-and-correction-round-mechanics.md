---
title: "Legion implementer handoff, push, and correction-round mechanics"
category: legion
tags:
  - legion-worker
  - handoff
  - jj
  - correction-round
  - pr-body
date: 2026-09-23
status: active
module: legion-worker
related_issues:
  - "S393260357-1"
  - "sjawhar/legion-smoke#89"
---

# Legion implementer handoff, push, and correction-round mechanics

Reusable mechanics learned while driving a Legion implementer phase (issue S393260357-1,
PR sjawhar/legion-smoke#89) through initial implementation plus three review/correction rounds.
The product change was trivial — appending one line, `Legion Stage 3 smoke proof.`, to
`README.md` — so every lesson below is about the workflow machinery, not the feature.

## Handoff file names follow the role, not the phase noun

`legion handoff complete` looks up the durable handoff by the pane's `LEGION_ROLE`
(`implementer` → `.legion/implementer.json`). `legion handoff write` names the file after its
`--phase` argument, and the accepted phase values are the role names — `planner`,
`implementer`, `tester`, `reviewer`:

- `legion handoff write --phase implement` silently writes `.legion/implement.json`, and
  `legion handoff complete` then fails with `.legion/implementer.json is missing from the
  workspace`. Use `--phase implementer`.
- `legion handoff write --phase retro` is rejected at the usage line (exit 2). Retro writes no
  handoff file at all; it reports completion with `legion handoff complete` alone.
- The `phase` field must NOT appear inside `--data`; it is supplied by `--phase`. Passing it in
  the JSON fails with `handoff data field phase is not allowed`.

## `legion handoff complete` requires the handoff in a committed parent, not the working copy

In jj the working copy is itself a commit. If `.legion/<role>.json` is only the change in the
working-copy commit `@`, `legion handoff complete` fails with `.legion/<role>.json is not
committed on the pane workspace`. The fix mirrors the skill's `jj split` step: after the handoff
is its own described commit, run `jj -R "$LEGION_WORKSPACE" new` so `@` becomes an empty child
and the handoff sits in `@-` (a real committed parent). Then `legion handoff complete` succeeds.

## Add the handoff as a child of the pushed commit; never rewrite the pushed commit

Writing `.legion/implementer.json` directly into the same working-copy commit that holds the
README change amends that commit, changing its SHA and orphaning the already-pushed commit (and
its green CI runs). Instead create the handoff commit as a child of the pushed commit:
`jj -R "$LEGION_WORKSPACE" new <pushed-readme-sha> -m "implementer: record handoff"`, drop the
handoff file in, then push — a fast-forward that preserves the README commit SHA and its CI. A
local rewrite that moved the bookmark sideways needs `jj bookmark set … --allow-backwards`
before the fast-forward push.

## The `legion gh` credential grant is single-use per bash command

Each bash tool call mints one credential grant; only the FIRST `legion gh` (or `jj git push`) in
that command redeems it. A second `legion gh` later in the same `&&` chain fails with
`403 GRANT_USED`. When a proof needs both a positive and a negative check against GitHub-served
content, fetch once into a shell variable and run both greps locally:
`R="$(legion gh -- api '…/contents/README.md?ref=<sha>' --jq .content | base64 -d)"; printf '%s\n' "$R" | grep -F …`.

## A review that requests changes with no thread, when acceptance is already met

Across three correction rounds the scripted reviewer submitted `CHANGES_REQUESTED` with no inline
threads (`legion threads resolve --pr <n> --repo <o>/<r>` printed `no unresolved threads` every
round) and every tester re-check passed. The acceptance criteria pinned the README to exactly one
added line, so no further content edit was possible without breaking them. The correct minimal
response each round was a new `.legion/implementer.json` handoff commit plus a refreshed PR body
answering the review — a documentation-only corrective push that leaves the product diff (and the
verified fingerprint) byte-for-byte unchanged. Do not invent a content change to "have a diff".

## Process gap: keep the PR body current, and mirror the tester proof into it

The READY PR body drifted stale: after four review rounds it still cited the round-1 head SHA,
still said "No review round yet", and never carried an `E2E (tester)` line even though the tester
recorded a verified proof in `.legion/tester.json`. The reviewer and merger read facts from the
PR body on GitHub, not from handoffs, so a stale body hides real verification. Every phase that
changes the head MUST refresh the body's `CI`, `Threads`, head SHAs, and its own `E2E` line; the
tester in particular MUST add the `E2E (tester)` line, not only the handoff. In a real (non-smoke)
run a missing `E2E (tester)` line sends the issue back to the tester before READY.
