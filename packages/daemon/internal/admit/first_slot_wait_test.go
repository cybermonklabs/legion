package admit

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// An issue the waiting line holds for its first phase is a wait of the daemon's own, so a tree
// every quiet issue of which is one of those is never told as stopped (dispatch://INFRA-2560,
// R5).
//
// The shape is the commonest one a tree waiting for the cap takes, and it is not the owed-start
// shape beside it: an issue waiting for its FIRST phase holds no start row at all, because the
// start is written by the promotion that grants the slot (the LineFirstPhase arm of the
// admission pass, which calls Engine.StartFirstPhase). So nothing of that issue is in
// record.LineFacts.Owed, and a rule that reads the cap's hold from the owed row alone finds
// this issue held by nothing and tells its tree.
//
// The tree below is a root standing at admitted whose design gate is open and whose one
// released child waits in the line: the root is the children-first wait, the child is the cap's
// own, and nothing of the tree has stopped. It is told as stopped at this head, which is the
// defect this test pins (measured on the live INFRA deployment at 2026-11-11T16:36Z: eight
// trees holding a slot stood in exactly that shape).
//
// The test is written over the behaviour and not over the predicate: it seeds the tree, runs an
// admission pass past the bound and reads what the controller is told, so a later rule that
// narrows the cap's hold again fails it however the narrowing is spelled. The plain stopped
// tree beside it is the control that says the pass ran and still names a tree that has stopped.
func TestAChildWaitingForItsFirstSlotIsAWaitByDesign(t *testing.T) {
	pool := migratedPool(t)
	bound := deploymentBound(t, "")
	// The cap is the slots the fixture holds, so the child below waits in the line for a slot
	// the cap cannot free, which is the wait this test is about.
	admission := quiet(t, 2)
	admission.UseStalledTreeNotice(bound)
	registerController(t, pool)
	records := claimStore(t, pool)

	// The tree whose every quiet issue waits by design: its root waits for its children and its
	// child waits for the cap.
	const waiting, firstSlot = "LEGION-760", "LEGION-761"
	seedQuietRoot(t, pool, waiting, "A", phase.Admitted)
	approved := 1
	seedGate(t, pool, record.DesignGate{Issue: waiting, ArtifactID: "artifact-760", LatestVersion: 1,
		ApprovedVersion: &approved, EverOpened: true})
	seedQuietChild(t, pool, waiting, firstSlot, "A1", phase.Admitted, "todo")
	seedMirror(t, pool, waiting, "in_progress")
	seedMirrorChild(t, pool, firstSlot, "todo", waiting)
	seedClaim(t, records, waiting, waiting, claim.RoleArchitect, supervise.StateIdle)

	// The control: a tree with none of the four waits, which the same pass must still name.
	const stopped = "LEGION-770"
	seedQuietRoot(t, pool, stopped, "B", phase.Implementing)
	seedClaim(t, records, stopped, stopped, claim.RoleArchitect, supervise.StateIdle)

	// The premise of the case, read from the facts the pass itself judges from: the child owes
	// no start row and holds no slot, so the owed-start half of the cap's hold does not name it.
	facts, slots := passFacts(t, pool)
	if _, owes := facts.Owed[firstSlot]; owes {
		t.Fatalf("%s owes a start row, so this fixture is not an issue waiting for its first phase: %v", firstSlot, facts.Owed)
	}
	for _, slot := range slots {
		if slot.Issue == firstSlot {
			t.Fatalf("%s holds one of the cap's rows, so it is not waiting in the line: %#v", firstSlot, slots)
		}
	}

	settled := settleMovements(t, pool, waiting, firstSlot, stopped)
	admission.now = func() time.Time { return settled.Add(bound + time.Hour) }

	tick(t, pool, admission, "past the bound")

	var told []string
	for _, notice := range stalledNotices(t, pool) {
		told = append(told, notice.issue)
	}
	if len(told) != 1 || told[0] != stopped {
		t.Fatalf("the controller was told of %v, want %v alone: %s waits for its children and %s waits in the line for its first slot, which is the cap's own hold",
			told, []string{stopped}, waiting, firstSlot)
	}
}
