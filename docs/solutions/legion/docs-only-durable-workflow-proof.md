---
title: "Running a docs-only change through the full Legion durable workflow"
category: legion
tags:
  - docs-only-pr
  - e2e-proof
  - review-rounds
  - ci-heads
  - negative-control
date: 2026-09-27
status: active
module: legion-worker
related_issues:
  - "S392719464-1"
  - "sjawhar/legion-smoke#406"
---

# Running a docs-only change through the full Legion durable workflow

Issue S392719464-1 took a one-file markdown fixture (`smoke/S392719464-1.md`) through
plan → implement → test → review×3 → retro. The file content is inconsequential; the
reusable value is how each phase treats a docs-only surface and how corrective review
rounds are proven. These patterns generalize to any docs-only PR and any change whose
only observable surface is a file read.

## Gate on "is there runtime code?", not "is there a diff?"

A markdown-only diff legitimately skips `ce-simplify-code` and the thermonuclear pair,
and the reviewer omits the `Thermo` line entirely. Record the skip explicitly instead of
silently omitting it — the implement handoff carried
`thermo: "SKIPPED — docs-only markdown diff; no runtime code, so ce-simplify-code and the thermonuclear pair do not run."`
Writing the reason down keeps the phase artifact auditable and stops a later reviewer
from flagging a missing step.

## Prove a file-read surface with a contents-API read plus a 404 negative control

The production-like surface for a repository file is the GitHub contents API at the exact
head SHA:

```bash
legion gh -- api "repos/<owner>/<repo>/contents/<path>?ref=<sha>" --jq .content | base64 -d
```

Pair every positive read with a negative control on a sibling path that must not exist
(`<name>-nope.md` → HTTP 404). The 404 proves the read surface distinguishes presence
from absence, so a passing read is not a false positive from a tool that returns success
unconditionally. Both the implementer's `E2E (implementer)` line and the tester's
`E2E (tester)` line use this same command with the 404 control.

## Prove each corrective append is *genuine*, not just the final state

When a review round asks for an appended line, do not merely observe the final file. Read
at the new head **and** compare to the prior head's line count. The tester's round-3 proof
recorded: "prior head c4ca002c7c9c had only three lines, so the fourth line was added by
the round-3 corrective push, not pre-existing." A snapshot of the final file cannot
distinguish "the fix landed" from "it was always there"; the prior-head comparison is what
actually verifies the round did work.

## Each corrective round produces two heads — cite CI at the one that gets merged

Every round is: append the line, `jj split` a **code-head** commit, `legion push` (full CI),
then `jj split` the **handoff** commit, `legion push` again (full CI). That yields a distinct
code head (e.g. `fe3a8bd357b8`) and handoff head (e.g. `eae97787f158`), each triggering the
`ci`/`check` and `fail-on-demand`/`gate` jobs. The PR body's `CI` and `E2E` lines must name
the **final head** — the handoff head is what the reviewer approves and what gets merged, so
citing CI only at the code head leaves the merged head unproven. Re-run CI each round rather
than reusing an earlier round's run ids. Note: `legion push` only applies the `skip-checks`
trailer for the planner/tester/review-changes_requested handoffs; an `implement.json` push
always runs full CI.

## Rewrite the PR body from a template each round, don't surgically edit it

Re-citing a new head touches the `CI` line and both `E2E` lines at once. A single mis-scoped
line edit clobbered the `E2E (tester)` line during round 1 (it had to be restored). Keep a
full READY-format body template and rewrite the whole file each round with the new SHA — it
is less error-prone than surgical line edits across a multi-line E2E block.

## Empty review threads are a terminal state, not a blocker

When the reviewer posts top-level `CHANGES_REQUESTED` bodies with no inline comments,
`legion threads resolve --pr <n> --repo <owner>/<repo>` correctly reports
`no unresolved threads` and the GraphQL `reviewThreads` list is `[]`. Report `0 resolved,
0 unresolved`; do not treat missing inline threads as an error or stall waiting for threads
to resolve. Still run `legion threads resolve` before every review-answering push — its
`no unresolved threads` output is the evidence.

## Don't author a placeholder test for a read-observable surface

The only failure mode for this fixture is "file absent or wrong content", already covered by
the E2E read plus the 404 control. A red regression test would be theater. The plan said so
(`plan.testEvidence.regressionLock`) and the tester honored it: the file read is the
regression lock. Force a test only where a plausible bug would actually fail it.
