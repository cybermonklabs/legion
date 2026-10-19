---
title: "A corrective implementing round starts from the remote tip, and review-round text is not plan-pinnable"
category: legion
tags:
  - jj
  - push
  - review-rounds
  - handoff
  - implementer
date: 2026-10-18
status: active
module: legion-worker
related_issues:
  - "S392473105-1"
  - "sjawhar/legion-smoke#619"
---

# A corrective implementing round starts from the remote tip, and review-round text is not plan-pinnable

## Base a corrective round on `legion/<KEY>@origin`, not your local chain

When a review sends the issue back to `implementing`, the daemon starts you in the same session
whose working copy still points at the chain you pushed last round. That chain is **stale**: between
your completion and your next task, the tester and the reviewer each committed their own handoff
(`test.json`, `review.json`) on top of your code commit and pushed, so `legion/<KEY>@origin` has
moved several commits ahead of your local `@-`.

- Before the round's first edit, `jj -R "$LEGION_WORKSPACE" git fetch`, then start the new work on
  the fetched tip: `jj -R "$LEGION_WORKSPACE" new 'legion/<KEY>@origin'`. Do not build on the
  working-copy commit the daemon resumed you at.
- If you edit on the stale chain instead, your `jj git push` is a non-fast-forward the daemon
  refuses (the remote bookmark moved); forcing is never the fix. Rebasing onto the tip is correct,
  but starting fresh on the tip avoids the detour entirely.
- Read the file you are about to change **after** `jj new …@origin`, not from memory of last round:
  the tip already carries every prior round's appended line, so an append built from a remembered
  earlier state would duplicate or drop lines.

## A review round's requested line text and count are the reviewer's, not the plan's

A plan that pre-pins the exact text or number of review-round edits is guessing at a decision the
reviewer owns at review time. Treat such a plan field as a default to be overridden, and take the
literal string and the number of rounds from the actual review each round.

- Append exactly the line the current review names, verbatim, and only that line; do not reuse a
  string the plan predicted or a sibling run used.
- Do not cap the number of corrective rounds from the plan: each `REQUEST_CHANGES` is its own
  implementing round with its own handoff and `handoff_complete`; a push alone does not finish one.

## Evidence

S392473105-1 (`sjawhar/legion-smoke#619`) added `smoke/S392473105-1.md` (bare key, line 1), then
three review rounds each appended one line. The planner's `plan.json` `reviewScope` pinned "at most
ONE appended line, text `Stage 3 review round 1 (S392473105)`" and flagged the conflict with the
repo's 3-round convention to the architect; the run overrode both — three rounds, and the actual
requested text was `Legion proof review round N (S392473105)`.

Rounds 2 and 3 each began with the remote tip already carrying the reviewer's and tester's handoffs
stacked above the previous round's code commit (round 3's tip was `00f5024f`, four commits above
the round-2 code commit `204856ae`). Each round was therefore started with
`jj new legion/S392473105-1@origin`, the one line appended, and the code commit pushed
(`cd6d0449` → `204856ae` → `5fd30b89`), every push a clean fast-forward and every code head green
on `ci / check` and `fail-on-demand / gate`.
