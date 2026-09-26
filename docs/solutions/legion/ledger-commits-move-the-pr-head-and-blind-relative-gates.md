---
title: "Legion ledger commits move the PR head and blind relative gates"
category: legion
tags:
  - legion-worker
  - legion-retro
  - handoff-ledger
  - pr-body
  - ci-line
  - merger
  - acceptance-criteria
  - fingerprint
date: 2026-09-26
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-270"
  - "sjawhar/legion-smoke#319"
---

# Legion ledger commits move the PR head and blind relative gates

Distilled from PR #319 (issue LEGSMOKE-270, "Stage 4b proof tree 1: the whole workflow"): one
appended line to the repo-root `smoke.txt`. The product change was one commit (`e13c0204`), but
the branch reached review at its fourth head, because the planner, implementer, and tester each
committed a `.legion/<phase>.json` ledger file on top. Everything below follows from that gap
between the *product head* and the *current head*.

## Name the product head separately from the ledger heads

Chain above `main@origin 8dec2fa4` at approval time:

| head | content | Actions runs (push + pull_request, two workflows) |
| --- | --- | --- |
| `a9671447` | `plan: record handoff` | — (pushed before the PR existed) |
| `e13c0204` | the one product line | 36235836647, 36235836650, 36235857437, 36235857467 |
| `274091d0` | `implement: record handoff` | 36235961763, 36235961756, 36235964566, 36235964561 |
| `0612343c` | `test: record handoff` (APPROVED) | 36236270926, 36236270927, 36236273669, 36236273672 |

Twelve CI runs for `+1` line: both workflows are `on: [push, pull_request]` and every ledger push
re-triggers them. That cost is structural — each role must push its own handoff — so do not try
to avoid the pushes. Instead, make the two heads explicit:

- Record `productHead` (the commit that last changed a product file) beside `head` in every
  handoff. The tester on #319 did this unprompted; make it the convention. Undeclared handoff
  fields pass validation and reach the next worker.
- Immediately after a ledger push, re-read the product surface at the new head — here the
  contents API (`last line`, `sha256`, `wc -l`) and
  `jj diff --from main@origin --to <new-head> --summary '~(.legion | docs/solutions)'` — and cite
  both heads in the `E2E` line. It costs seconds and gives the next role an exact current-head anchor.
- Equivalence, not repetition: the unchanged-diff fingerprint from `skill://legion-worker`
  (*The unchanged-diff check*) already excludes `.legion` and `docs/solutions`. Identical
  fingerprints at `productHead` and the current head prove no product byte moved between them,
  which is what lets a CI line read "gates green at `<current head>`; product identical to
  `<productHead>`, where both E2E proofs ran" without re-running the real-surface proof.

## The last pusher before review refreshes the `CI:` line

On #319 the PR body's `CI:` line cited the runs at `e13c0204` and `274091d0`. The tester's ledger
push created `0612343c`; its runs appeared only in the reviewer's review body, and the reviewer
raised exactly that as its fast-follow. The reviewer never pushes, so it cannot be the one to fix
it.

Rule: whichever role pushes last before a review round (normally the tester) re-cites the `CI:`
line at the head it produced — `legion gh -- run list --repo <o>/<r> --commit <head> --json
databaseId,name,event,conclusion,headSha`, then `legion gh -- pr edit <n> --body-file …`. The
reviewer verifies run ids at the head it approves and cites them in its review body as well.

## A reviewer finding that is not `REQUEST_CHANGES` must land where a later role reads it

The review body ended with a fast-follow (cite the approved head's run ids in the `CI:` line)
while the PR body still said `Fast-follow: none` and `Threads: 0 resolved, 0 unresolved`. The
merger publishes READY from the body's gate facts and `legion threads resolve` tracks only review
threads, so a fast-follow that lives only in review prose has no owner.

Rule: open it as a review thread (then the implementer answers it and `legion threads resolve`
accounts for it), or direct the implementer to write it into the body's `Fast-follow:` field
before READY. A body that says `none` while the review names one is a body that lies to the merger.
On #319 the revived implementer applied the wording fix to the body during retro (a body edit is
not a commit and does not move the approved head).

## An approval on a head that still carries `.legion/` is invisible to every downstream gate

The reviewer approved `0612343c` on a targeted operator instruction, and said so in the review
body: "this head still carries `.legion/` handoffs, which the ordinary gate would have deleted
first." From that point:

- Retro cannot repair it. Anything above the approved head other than `docs/solutions/` voids the
  approval, and retro's own precondition ("`.legion/` was deleted before retro") is simply false.
  Retro records the fact and proceeds; it never deletes `.legion/` on its own.
- The merger's census in `skill://legion-worker` is relative: `jj diff --from <approved-sha> --to
  <tip-sha> --summary`, and the same with `'~docs/solutions'` printing nothing. Both pass while
  three ledger files sit *inside* the approved head. The base of this very PR, `8dec2fa4` "proof
  fixture: remove the handoffs and learnings Stage 3 runs merged (#313)", is the human cleanup
  commit from the last time this leaked (and `f61f2c99` #307 the time before).

Rules:

1. A reviewer that approves a head the ordinary cleanup would have rejected enumerates in the
   review body the exact non-product paths its approval blesses, and tells the architect that the
   `.legion/` deletion step now has no owner.
2. The merger quotes an *absolute* census in READY beside the relative one:
   `jj diff --from main@origin --to <tip-sha> --summary`. If it lists `.legion/`, READY names the
   post-merge cleanup that a human will have to do (as #313 and #307 did), so the leak is a
   decision and not a surprise.

## Phrase "touches exactly one file" against a fileset and a head

The spec's Acceptance 2 read "`gh pr diff <n> --name-only` lists only `smoke.txt`". Every in-flight
head lists `.legion/<phase>.json` too, and the retro commit adds `docs/solutions/`. The planner had
to invent a `processArtifactExemption`, and then the planner, implementer, and tester each
re-reported the same exemption to the architect in their handoffs — three handoffs, one unchanged
spec. The same finding had already been reported on the earlier tree LEGSMOKE-266.

Rules:

- Spec authors write file-count acceptance as a machine check that holds at every head:
  `jj diff --from main@origin --to <head> --summary '~(.legion | docs/solutions)'` prints exactly
  `M smoke.txt`. The fileset must be a single expression — jj unions positional filesets, so
  `'~.legion' '~docs/solutions'` excludes nothing.
- A planner writing the same interpretation for a second issue escalates it as a spec change
  (`dispatch_suggest` on the spec's acceptance line, or a `dispatch_ask`), not as another
  `reportedToArchitect` note. A handoff note reaches one tree's architect and dies there.
