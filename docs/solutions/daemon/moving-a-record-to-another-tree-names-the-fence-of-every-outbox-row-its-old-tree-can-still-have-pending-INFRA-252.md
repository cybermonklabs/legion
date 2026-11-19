---
title: "Moving a record to another tree names the fence of every outbox row its old tree can still have pending"
category: daemon
tags:
  - outbox
  - fence
  - generation
  - adoption
date: 2026-10-26
status: active
module: packages/daemon
related_issues:
  - "INFRA-252"
  - "sjawhar/legion#2226"
---

# Moving a record to another tree names the fence of every outbox row its old tree can still have pending

- Code that moves an issue from one tree to another (adoption, hand-over, re-admission) lists
  every outbox kind its old tree can still have queued, and names the fence each one checks
  before it acts. Put that list in the function's doc comment.
- Bumping the issue's generation fences only the rows that compare the issue's generation. A row
  fenced on the old root's linger generation can match again once the new root lingers at the same
  number; give such a row the tree it was queued for and finish it without acting when the issue's
  tree differs.
- A stop row of the old tree (a `suspend`, a `tree_close`) is the exception: its fence keeps it
  off the new tree's run, not off a claim that still runs in the old tree. Such a claim holds the
  old tree's cleanup until something stops it, and the new tree's start, which would, waits for
  the issue's admission. The row still stops a claim the in-memory machine shows standing in the
  old tree, and leaves one the new tree re-pointed.
- A stop the old tree still has queued is not enough: one can have finished before the move
  (superseded by a newer start) or fall to the new generation's fence, and a start of the old tree
  already past its fences can bring a claim up after the move has read the claims. So the move
  itself queues a stop of the old tree for every role of the issue but one whose claim already
  stands in another tree, and the new tree's rows never read it, since it is queued at the old
  generation.
- Each row kind left unfenced is a test: queue it for the old tree, move the issue, run the row,
  and assert it acts on nothing.

## Evidence

`workflow/adopt.go` `Adopt` bumps the generation and names what that fences: the old tree's linger
close, its supervise rows and its issue suspend. `WorkspaceRemove` was fenced on the linger
generation alone, so an old linger's removal could delete an adopted child's workspace; it now
carries `Tree` (`record/outbox.go:400`), written by `workflow/linger.go` and checked in
`daemon/outbox.go` `removeWorkspace`, covered by `TestAWorkspaceRemovalOfATreeTheIssueLeftActsOnNothing`.
A `tree_close` of the tree the issue left is a stop of that tree (`stopInLeftTree`). Its only fence
is the in-memory claim standing in `payload.Tree`. It reserves nothing; the close rows of the issues
still in that tree reserve and confirm the cleanup.

A hand-over that landed before the linger's suspend of a child's planner left that planner
`registered` in the old tree, and the old tree's cleanup waited on it until the child's new tree
started it, which a child held on a blocker never did. `daemon/outbox.go` `stopInLeftTree` now
stops it from the old tree's own `suspend` and `tree_close` rows, and wakes that tree's close rows
so the cleanup confirms on the stop (`TestTheOldTreesStopsReachACarriedRoleItsLingerSuspendMissed`).
A merged child whose linger suspend a production check's look start superseded, carried while the
look ran, had no stop row left at all, and neither had a claim that look's in-flight start brought
up after the move; `workflow.LeftTreeStops`, queued by admission's `readmit` and `moveIntoTree` for
every role, covers both (`TestAMoveStopsTheCarriedRoleNoStopRowOfItsOldTreeStillReaches`).
