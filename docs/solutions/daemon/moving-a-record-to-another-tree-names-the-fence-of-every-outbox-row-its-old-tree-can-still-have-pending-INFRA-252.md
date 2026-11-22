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
  - "cybermonklabs/legion#4450"
---

# Moving a record to another tree names the fence of every outbox row its old tree can still have pending

- Code that moves an issue from one tree to another (adoption, hand-over, re-admission) lists
  every outbox kind its old tree can still have queued, and names the fence each one checks
  before it acts. Put that list in the function's doc comment.
- Bumping the issue's generation fences only the rows that compare the issue's generation. A row
  fenced on the old root's linger generation can match again once the new root lingers at the same
  number; give such a row the tree it was queued for, and check that tree against the issue's
  before any linger fence.
- A row of the tree the issue left that removes what the new tree now uses, its workspace,
  finishes without acting. A suspend or tree close of that tree still acts: a carried child's role
  keeps running in its old tree until it is stopped or its new tree's start re-points it, and the
  old tree's cleanup waits on it. Such a row stops the issue's claim while the claim stands in the
  row's tree, and finishes without acting once the claim is in another tree.
- The move itself queues the old tree's suspend of every role of the issue: a stop row of the old
  tree can have finished without stopping the role (a newer start superseded it), and a new tree
  held before its first start stops nothing.
- Each row kind the generation does not fence is a test: queue it for the old tree, move the issue,
  run the row, and assert it acts on nothing of the new tree. For a stop row, also assert that it
  stops the claim still standing in the old tree and leaves running one the new tree re-pointed.

## Evidence

`workflow/adopt.go` `moveIntoTree`, which both adoption routes call (`Adopt`, `AdoptMember`),
writes the member's next generation, and its doc comment names the old tree's suspend and
tree-close rows as rows that generation does not fence (`workflow/adopt.go:240-243`). Before that
write it queues the old tree's suspend of every role (`StopLeftTree`, `workflow/adopt.go:267-271`
and `:286-293`), and a re-admission of a member leaving another tree, a hand-over or an orphan,
does the same (`admit/admit.go:1327-1333`). `daemon/outbox.go` `supervise` sends a suspend or tree
close whose tree is not the issue's to `stopInLeftTree` before any generation or linger fence
(`daemon/outbox.go:2343-2345`; `record/outbox.go:1072-1080` names the exception). That function
stops the claim only while its machine stands in the row's tree, waits while a stored claim with no
machine here may still run there, and otherwise finishes without acting
(`daemon/outbox.go:2960-2992`). `TestTheOldTreesStopsReachACarriedRoleItsLingerSuspendMissed`,
`TestALeftTreeStopWaitsForTheMachineOfAStoredClaimInItsTree` and
`TestAMoveStopsTheCarriedRoleNoStopRowOfItsOldTreeStillReaches`
(`daemon/outbox_tree_cleanup_test.go`), and `TestAnAdoptionQueuesTheOldTreesStopOfEachClaimItLeavesThere`
(`workflow/left_tree_stops_test.go`) cover them.

`WorkspaceRemove` was fenced on the linger generation alone, so an old linger's removal could delete
an adopted child's workspace; it now carries `Tree` (`record/outbox.go:1317-1320`), written by
`workflow/linger.go:476` and checked in `daemon/outbox.go` `removeWorkspace` (`:3966-3970`), covered
by `TestAWorkspaceRemovalOfATreeTheIssueLeftActsOnNothing` (`daemon/outbox_test.go:1950`).
