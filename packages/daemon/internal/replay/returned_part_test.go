package replay

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// returnedPartWindow is a window from 09:00 to 14:00 under a cap of two, its inputs reaching back
// to the daemon's boot at 06:00, that holds the three ways an issue in flight comes to hold no
// slot in a phase that keeps one.
//
// Before the start ACME-1, a root, is promoted at 06:05, plans from 06:20 and implements from
// 06:50. Its implementer's turn ends at 08:30 with no move, and at 08:40 a pass takes its slot
// back with no wait and no move. Nothing gives it a slot again or moves it to the window's end: it
// owes no start and carries no mark of a release for a wait, so the daemon's line holds it.
//
// In the window ACME-2 and ACME-3, roots, take the two slots at 09:05 and 09:10, plan from 09:20
// and 09:25 and implement from 09:50 and 09:55. ACME-4 and ACME-6, roots handed to Legion at 09:15
// and 09:30, wait for a slot, and ACME-5, an issue Legion is not given, stands in backlog from
// 09:16. At 10:00 a person links ACME-3 as blocked by ACME-5: a pass gives ACME-3's slot back for
// the wait, and ACME-4 takes it. ACME-2 moves past its review at 11:00, and ACME-6 takes its slot.
// At 11:30 ACME-2's architect sends it back from accepting to implementing at a full cap, so it
// owes its implementer's start. At 11:40 the person closes ACME-5, which ends ACME-3's wait at a
// full cap. ACME-4 moves past its review at 12:00 and ACME-3 takes its slot, ahead of ACME-2, since
// it gave its slot back first; ACME-6 moves past its review at 12:30, and ACME-2 takes its slot and
// its implementer starts.
func returnedPartWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(14, 0))
	b.reaches(3 * time.Hour)
	b.capped(2)
	b.boot(clock(6, 0))
	promotedRoot(b, clock(6, 5), "ACME-1", "a1")
	b.rootDesigned(clock(6, 20), "ACME-1")
	b.segmentEnds(clock(6, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.turnEnds(clock(8, 30), "ACME-1", claim.RoleImplementer)
	b.slotReleased(clock(8, 40), "ACME-1")

	promotedRoot(b, clock(9, 5), "ACME-2", "a2")
	promotedRoot(b, clock(9, 10), "ACME-3", "a3")
	b.file(clock(9, 15), "ACME-4", filing{Title: "Ship the ACME-4 change", Status: "todo", Rank: "a4", Labels: handedToLegion})
	b.file(clock(9, 16), "ACME-5", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "a5"})
	b.rootDesigned(clock(9, 20), "ACME-2")
	b.rootDesigned(clock(9, 25), "ACME-3")
	b.file(clock(9, 30), "ACME-6", filing{Title: "Ship the ACME-6 change", Status: "todo", Rank: "a6", Labels: handedToLegion})
	b.segmentEnds(clock(9, 50), "ACME-2", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(9, 55), "ACME-3", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.edit(clock(10, 0), "ACME-3", func(issue *builtIssue) { issue.BlockedBy = []string{"ACME-5"} })
	b.slotReleased(clock(10, 0), "ACME-3")
	b.rootPromoted(clock(10, 0), "ACME-4")
	b.rootDesigned(clock(10, 15), "ACME-4")
	b.segmentEnds(clock(10, 20), "ACME-2", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(10, 40), "ACME-2", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(10, 45), "ACME-4", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(11, 0), "ACME-2", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.rootPromoted(clock(11, 0), "ACME-6")
	b.rootDesigned(clock(11, 15), "ACME-6")
	b.segmentEnds(clock(11, 15), "ACME-4", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.turnEnds(clock(11, 30), "ACME-2", claim.RoleArchitect)
	b.moved(clock(11, 30), "ACME-2", phase.Accepting, phase.Implementing, workflow.TriggerBackward)
	b.segmentEnds(clock(11, 35), "ACME-4", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(11, 35), "ACME-6", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.edit(clock(11, 40), "ACME-5", setStatus(record.DoneStatus))
	b.segmentEnds(clock(11, 50), "ACME-6", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(12, 0), "ACME-4", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.waitEnded(clock(12, 0), "ACME-3", phase.Implementing)
	b.segmentEnds(clock(12, 10), "ACME-6", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.segmentEnds(clock(12, 30), "ACME-6", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.workCameBack(clock(12, 30), "ACME-2", phase.Implementing)
	b.turnStarts(clock(12, 30), "ACME-2", claim.RoleImplementer)
	return b.build()
}

// The record's scorecard counts an issue in flight that holds no slot as started work that waits
// for one only where the daemon's line places it in the returned part (record.Waiting): where it
// owes the start of the move into its phase, or where a pass gave its slot back for a wait, and
// never where it holds neither, which the line holds. Over returnedPartWindow, five hours:
//
//	ACME-1: in flight all five hours, held from the start to the end in no part of the line: 0
//	hours in reading (b) and 0 in reading (a).
//	ACME-2: in flight from 09:05; owes its implementer's start from its send-back at 11:30 and
//	waits only for a slot to 12:30: 1 hour in each reading.
//	ACME-3: in flight from 09:10; given back for its wait at 10:00, blocked to 11:40, then waits
//	only for a slot to 12:00: 2 hours in reading (b), 20 minutes in reading (a).
//
// Reading (b): 3 hours, an average of 0.6, at most 2 (11:30 to 12:00). Reading (a): 4/3 hours, an
// average of 0.26666…, at most 2 (11:40 to 12:00).
func TestAnIssueWithNoSlotWaitsForOneOnlyWhereTheLinePlacesIt(t *testing.T) {
	t.Parallel()
	card := assertRecordedHours(t, returnedPartWindow(t), []issueHours{
		{key: "ACME-1", inFlight: 5},
		{key: "ACME-2", inFlight: 4 + 55.0/60, b: 1, a: 1},
		{key: "ACME-3", inFlight: 4 + 50.0/60, b: 2, a: 1.0 / 3},
	})
	assertOccupancy(t, "startedWithoutSlot", card.StartedWithoutSlot, Occupancy{Average: 0.6, Max: 2, Hours: 3})
	assertOccupancy(t, "waitOnlyForSlot", card.WaitOnlyForSlot, Occupancy{Average: 4.0 / 15, Max: 2, Hours: 4.0 / 3})
}

// restartAndTreeWindow is a window from 09:00 to 13:00 under a cap of one, its inputs reaching
// back to the daemon's boot at 06:00, that holds the two other halves of the rule: a held phase's
// retry, which writes its worker's start as a move does, and a tree that stops being live.
//
// Before the start ACME-1, a root, is promoted at 06:05, plans from 06:20 and implements from
// 06:50. At 08:40 a pass takes its slot back with no wait and no move, and at 08:50 its
// implementer's claim fails, which holds it from implementing.
//
// In the window ACME-2, a root, takes the slot at 09:05, and its architect files ACME-3 under it
// in triage at 09:15, which the daemon releases at 09:16. ACME-4, an issue Legion is not given,
// stands in backlog from 09:10, and ACME-5, a root handed to Legion at 09:30, waits for a slot. At
// 09:20 ACME-2's architect registers the design: ACME-2 waits for its open child and gives its
// slot back, and ACME-3 takes it and plans, implementing from 09:50. At 10:00 a person links ACME-3
// as blocked by ACME-4: a pass gives ACME-3's slot back for the wait, and ACME-5 takes it. At 10:30
// ACME-1's architect retries its held phase at a full cap, so it owes its implementer's start. At
// 11:20 a person closes ACME-2: it leaves the workflow and lingers to 13:20, and from then its tree
// is not live. At 11:40 the person closes ACME-4, which ends ACME-3's wait in that tree. ACME-5
// moves past its review at 12:00, and ACME-1 takes its slot and its implementer starts.
func restartAndTreeWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.reaches(3 * time.Hour)
	b.capped(1)
	b.boot(clock(6, 0))
	promotedRoot(b, clock(6, 5), "ACME-1", "a1")
	b.rootDesigned(clock(6, 20), "ACME-1")
	b.segmentEnds(clock(6, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.slotReleased(clock(8, 40), "ACME-1")
	b.claimFailed(clock(8, 50), "ACME-1", claim.RoleImplementer)

	promotedRoot(b, clock(9, 5), "ACME-2", "a2")
	b.file(clock(9, 10), "ACME-4", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "a4"})
	b.filedBy(clock(9, 15), "ACME-3", filing{Title: "Ship the ACME-3 change", Parent: "ACME-2", Tree: "ACME-2", Status: "triage", Rank: "a3"},
		b.session("ACME-2", claim.RoleArchitect))
	b.daemonWrites(clock(9, 16), "ACME-3", "todo")
	b.rootWaitsForChildren(clock(9, 20), "ACME-2", 1)
	b.childStarts(clock(9, 20), "ACME-3")
	b.file(clock(9, 30), "ACME-5", filing{Title: "Ship the ACME-5 change", Status: "todo", Rank: "a5", Labels: handedToLegion})
	b.segmentEnds(clock(9, 50), "ACME-3", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.edit(clock(10, 0), "ACME-3", func(issue *builtIssue) { issue.BlockedBy = []string{"ACME-4"} })
	b.slotReleased(clock(10, 0), "ACME-3")
	b.rootPromoted(clock(10, 0), "ACME-5")
	b.rootDesigned(clock(10, 15), "ACME-5")
	b.info(clock(10, 30), "workflow: a held phase is released", "issue", "ACME-1", "phase", string(phase.Implementing), "reason", "retry held phase")
	b.segmentEnds(clock(10, 45), "ACME-5", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(11, 15), "ACME-5", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.edit(clock(11, 20), "ACME-2", setStatus(record.DoneStatus))
	b.leaves(clock(11, 20), "ACME-2", record.DoneStatus)
	b.segmentEnds(clock(11, 35), "ACME-5", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.edit(clock(11, 40), "ACME-4", setStatus(record.DoneStatus))
	b.segmentEnds(clock(12, 0), "ACME-5", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.workCameBack(clock(12, 0), "ACME-1", phase.Implementing)
	b.launched(clock(12, 0), "ACME-1", claim.RoleImplementer, true)
	b.turnStarts(clock(12, 0), "ACME-1", claim.RoleImplementer)
	return b.build()
}

// The line reading asks the two other halves of the daemon's rule (record.Waiting's first loop):
// a held phase's retry writes its worker's start as a move does, so an issue that holds no slot
// owes it from the retry; and an issue of a tree that is not live, or whose root lingers, needs no
// slot (NeedsSlot), so the line holds it whatever it owes or waits on. Over restartAndTreeWindow,
// four hours:
//
//	ACME-1: in flight all four hours; held to 10:30, then owes its implementer's start and waits
//	only for a slot to 12:00: 1.5 hours in each reading.
//	ACME-2: in flight from 09:05 to its leave at 11:20; given back for its wait on its child at
//	09:20 and blocked to its leave: 2 hours in reading (b), 0 in reading (a).
//	ACME-3: in flight from 09:20; given back for its wait at 10:00 and blocked to 11:20, where its
//	tree's root leaves and lingers, and in no part of the line after, since its tree needs no slot
//	and a pass clears the mark of its release, though its wait goes on to 11:40: 1 hour 20 minutes
//	in reading (b), 0 in reading (a).
//
// Reading (b): 29/6 hours, an average of 29/24, at most 3 (10:30 to 11:20). Reading (a): 1.5 hours,
// an average of 0.375, at most 1.
func TestARetriedHoldOwesItsStartAndATreeThatIsNotLiveWaitsForNoSlot(t *testing.T) {
	t.Parallel()
	card := assertRecordedHours(t, restartAndTreeWindow(t), []issueHours{
		{key: "ACME-1", inFlight: 4, b: 1.5, a: 1.5},
		{key: "ACME-2", inFlight: 2.25, b: 2},
		{key: "ACME-3", inFlight: 11.0 / 3, b: 4.0 / 3},
	})
	assertOccupancy(t, "startedWithoutSlot", card.StartedWithoutSlot, Occupancy{Average: 29.0 / 24, Max: 3, Hours: 29.0 / 6})
	assertOccupancy(t, "waitOnlyForSlot", card.WaitOnlyForSlot, Occupancy{Average: 0.375, Max: 1, Hours: 1.5})
}

// lingeringParentWindow is a window from 09:00 to 13:00 under a cap of two, recorded under main,
// in which a started parent waits on its open child while the root of its tree lingers. ACME-1, a
// root, takes a slot at 09:05, and its architect files ACME-2 under it in triage at 09:10, which
// the daemon releases at 09:11. At 09:20 the architect registers the design: ACME-1 waits for its
// open child and gives its slot back, and ACME-2 takes a slot and plans. At 10:00 a person files
// ACME-3 under ACME-2 in backlog, and at 10:10 ACME-4 under ACME-2 in todo, which takes the free
// slot and plans. At 10:30 the person moves ACME-4 to backlog: it leaves the workflow, its record
// parked at done. At 11:20 the person closes ACME-1: it leaves the workflow and lingers to 13:20,
// and ACME-2's slot goes back, its tree not live. At 11:40 the person closes ACME-3.
//
// Replayed under main, whose wait stops the phase under way since cybermonklabs/legion#4592
// (record.SlotRule.WaitStopsPhase), ACME-2 waits on its open child ACME-3 from 10:00, moves into
// awaiting_children, which keeps no slot, and its worker is stopped.
// ACME-4 is an open child of it from 10:10 to its leave at 10:30, and holds it no longer in
// backlog, since the daemon recorded it and its record has left the workflow (record.ChildOpen).
// From 11:20 the tree runs no longer, its root lingering, and the pass takes no parent of a tree
// that does not run back out of awaiting_children (admit's childrenWaits), so ACME-2 stands there
// to the window's end, though ACME-3 closes at 11:40. Four hours:
//
//	ACME-1: waits on its child from 09:20 to its leave at 11:20, given back and blocked all of it:
//	one wait of left_workflow for 2 hours, and 2 hours in reading (b).
//	ACME-2: waits on its children from 10:00 to ACME-3's close at 11:40: one wait of child_done for
//	1 hour 40 minutes; started before its review with no slot in awaiting_children from 10:00 to
//	13:00, its wait on its children included: 3 hours in reading (b).
//	ACME-4: holds a slot from 10:10 to its leave at 10:30 and waits on nothing.
func lingeringParentWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.capped(2)
	b.boot(clock(9, 0))
	promotedRoot(b, clock(9, 5), "ACME-1", "a1")
	b.filedBy(clock(9, 10), "ACME-2", filing{Title: "Ship the ACME-2 change", Parent: "ACME-1", Tree: "ACME-1", Status: "triage", Rank: "a2"},
		b.session("ACME-1", claim.RoleArchitect))
	b.daemonWrites(clock(9, 11), "ACME-2", "todo")
	b.rootWaitsForChildren(clock(9, 20), "ACME-1", 1)
	b.childStarts(clock(9, 20), "ACME-2")
	b.file(clock(10, 0), "ACME-3", filing{Title: "Ship the ACME-3 change", Parent: "ACME-2", Status: "backlog", Rank: "a3"})
	b.file(clock(10, 10), "ACME-4", filing{Title: "Ship the ACME-4 change", Parent: "ACME-2", Tree: "ACME-1", Status: "todo", Rank: "a4"})
	b.childStarts(clock(10, 10), "ACME-4")
	b.edit(clock(10, 30), "ACME-4", setStatus("backlog"))
	b.leaves(clock(10, 30), "ACME-4", "backlog")
	b.edit(clock(11, 20), "ACME-1", setStatus(record.DoneStatus))
	b.leaves(clock(11, 20), "ACME-1", record.DoneStatus)
	b.slotReleased(clock(11, 20), "ACME-2")
	b.edit(clock(11, 40), "ACME-3", setStatus(record.DoneStatus))
	return b.build()
}

// childrenAgainWindow is a window from 09:00 to 13:00 under a cap of two, recorded under main, in
// which a started parent waits on its children twice in one run of its tree, in two phases.
// ACME-1, a root, takes a slot at 09:05, and its architect files ACME-2 under it in triage at
// 09:10, which the daemon releases at 09:11. At 09:20 the architect registers the design: ACME-1
// waits for its open child and gives its slot back, and ACME-2 takes a slot and plans to 10:20,
// then implements. A person files ACME-3 under ACME-2 in backlog at 09:30 and closes it at 10:00,
// and files ACME-4 under it in backlog at 11:30 and closes it at 12:00.
//
// Replayed under main, whose wait stops the phase under way since cybermonklabs/legion#4592
// (record.SlotRule.WaitStopsPhase), ACME-2 waits on ACME-3 from 09:30, moving from planning into
// awaiting_children, its planner stopped, and comes back to planning at 10:00; it plans the rest
// of its segment and implements, and waits on ACME-4 from 11:30, moving from implementing into
// awaiting_children. Each move into the wait is a phase move the log holds. Four hours:
//
//	ACME-1: waits on its child from 09:20 to the window's end, given back and blocked all of it:
//	one wait, open, for 3 hours 40 minutes, and 3 hours 40 minutes in reading (b).
//	ACME-2: waits on ACME-3 from 09:30 to 10:00 and on ACME-4 from 11:30 to 12:00: two waits of
//	child_done for 1 hour, in awaiting_children with no slot all of it, 1 hour in reading (b).
func childrenAgainWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.capped(2)
	b.boot(clock(9, 0))
	promotedRoot(b, clock(9, 5), "ACME-1", "a1")
	b.filedBy(clock(9, 10), "ACME-2", filing{Title: "Ship the ACME-2 change", Parent: "ACME-1", Tree: "ACME-1", Status: "triage", Rank: "a2"},
		b.session("ACME-1", claim.RoleArchitect))
	b.daemonWrites(clock(9, 11), "ACME-2", "todo")
	b.rootWaitsForChildren(clock(9, 20), "ACME-1", 1)
	b.childStarts(clock(9, 20), "ACME-2")
	b.file(clock(9, 30), "ACME-3", filing{Title: "Ship the ACME-3 change", Parent: "ACME-2", Status: "backlog", Rank: "a3"})
	b.edit(clock(10, 0), "ACME-3", setStatus(record.DoneStatus))
	b.segmentEnds(clock(10, 20), "ACME-2", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.file(clock(11, 30), "ACME-4", filing{Title: "Ship the ACME-4 change", Parent: "ACME-2", Status: "backlog", Rank: "a4"})
	b.edit(clock(12, 0), "ACME-4", setStatus(record.DoneStatus))
	return b.build()
}

// linkPastReviewWindow is a window from 09:00 to 13:00 in which a link wait stands while its issue
// moves past its review. ACME-1, a root, is promoted at 09:05 and works each phase to reviewing at
// 10:20, its reviewer's turn running from then. ACME-2, an issue Legion is not given, stands in
// backlog from 09:10. At 10:30 a person links ACME-1 as blocked by ACME-2: a pass gives ACME-1's
// slot back for the wait, and its review goes on. At 10:50 the review approves ACME-1 into
// accepting, where the daemon judges no wait of it (blockers' waitsOf), and its architect's turn
// starts on no slot. At 11:30 the person closes ACME-2. Four hours: ACME-1 waits on ACME-2 from
// 10:30 to its move past its review at 10:50, one wait of moved_on for 20 minutes, given back and
// blocked all of it, 20 minutes in reading (b); and none from then, though ACME-2 is done only at
// 11:30.
func linkPastReviewWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.boot(clock(9, 0))
	promotedRoot(b, clock(9, 5), "ACME-1", "a1")
	b.file(clock(9, 10), "ACME-2", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "a2"})
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 40), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(10, 0), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(10, 20), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.edit(clock(10, 30), "ACME-1", func(issue *builtIssue) { issue.BlockedBy = []string{"ACME-2"} })
	b.slotReleased(clock(10, 30), "ACME-1")
	b.turnEnds(clock(10, 50), "ACME-1", claim.RoleReviewer)
	b.moved(clock(10, 50), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.turnStarts(clock(10, 50), "ACME-1", claim.RoleArchitect)
	b.edit(clock(11, 30), "ACME-2", setStatus(record.DoneStatus))
	return b.build()
}

// issueHours is the hours one issue of a hand-worked record stands in flight and in each reading
// of started work that waits for a slot: b, a member of the returned part with no slot, and a,
// such a member that admission.blocked does not name.
type issueHours struct {
	key            string
	inFlight, b, a float64
}

// assertRecordedHours fails for each issue of want whose hours in window's record are not its own,
// each read by the code Score reads them by over that issue's course of the record's line timeline
// alone, and for a fact of the record the trace could not place; it answers the record's scorecard.
func assertRecordedHours(t *testing.T, window Window, want []issueHours) Scorecard {
	t.Helper()
	trace := placedTrace(t, window)
	lined := recordLineTimeline(trace)
	for _, issue := range want {
		card := measureAlone(lined, issue.key)
		assertNear(t, issue.key+"'s inFlight.hours", card.InFlight.Hours, issue.inFlight)
		assertNear(t, issue.key+"'s startedWithoutSlot.hours", card.StartedWithoutSlot.Hours, issue.b)
		assertNear(t, issue.key+"'s waitOnlyForSlot.hours", card.WaitOnlyForSlot.Hours, issue.a)
	}
	return scoreTrace(trace, resolvePriorities(trace))
}

// assertSnapshotHours fails for each issue of want whose hours in either reading of started work
// that waits for a slot, in the snapshot timeline of result, are not its own: the hours the
// replay's own admission passes place it in the line for, each read over that issue's course
// alone. An issue's hours in flight are the record's alone and are not read here.
func assertSnapshotHours(t *testing.T, result RunResult, want []issueHours) {
	t.Helper()
	for _, issue := range want {
		card := measureAlone(result.snapshot, issue.key)
		assertNear(t, "the snapshot's "+issue.key+"'s startedWithoutSlot.hours", card.StartedWithoutSlot.Hours, issue.b)
		assertNear(t, "the snapshot's "+issue.key+"'s waitOnlyForSlot.hours", card.WaitOnlyForSlot.Hours, issue.a)
	}
}

// measureAlone is the Scorecard of key's course alone in timeline, over timeline's span.
func measureAlone(timeline Timeline, key string) Scorecard {
	alone := Timeline{From: timeline.From, To: timeline.To, Issues: map[string]*Course{}}
	if c := timeline.Issues[key]; c != nil {
		alone.Issues[key] = c
	}
	return Measure(alone)
}
