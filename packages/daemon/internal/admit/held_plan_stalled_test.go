package admit

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The two holds an owner's plan makes silence no tree (silencedTree). A parent held before its
// retro for the issues beneath it (children_before_retro) and a task that was stopped
// (task_stopped) each stand on an issue that has had its start and stands in held, which the
// rule reads as stopped before it reads any word (lineHolds.waitsByDesign, its fourth clause),
// as it reads a parent that waits in accepting for its children. So a quiet tree of either is
// told. The held parent's child waits on a Dispatch blocker, a wait by design, so the parent alone
// is what names its tree.
//
// The control is a tree whose root waits on a person, with a child at admitted that the same
// Dispatch blocker shape holds: every issue of it waits by design, so it is silent, and a pass
// that names the two trees of a plan's holds is a pass that still leaves a waiting tree alone.
func TestAHoldOfAPlanIsNoWaitByDesign(t *testing.T) {
	bound := deploymentBound(t, "")
	pool := migratedPool(t)
	admission := quiet(t, 4)
	admission.UseStalledTreeNotice(bound)
	registerController(t, pool)
	records := claimStore(t, pool)
	approved := 1
	treeOf := func(root, rank string, hold *record.Hold) {
		t.Helper()
		issue := quietRootRecord(root, rank, phase.Admitted)
		if hold != nil {
			issue.Phase, issue.Hold, issue.Status, issue.DispatchStatus = phase.Held, hold, "needs_review", "needs_review"
		}
		putIssue(t, pool, issue)
		seedLifecycle(t, pool, root)
		if record.KeepsSlot(issue) {
			takeSlot(t, pool, root)
		}
		seedGate(t, pool, record.DesignGate{Issue: root, ArtifactID: "artifact-" + root, LatestVersion: 1,
			ApprovedVersion: &approved, EverOpened: true})
		seedMirror(t, pool, root, issue.Status)
		seedClaim(t, records, root, root, claim.RoleArchitect, supervise.StateIdle)
	}
	blockedChild := func(root, child, rank, blocker string) {
		t.Helper()
		seedQuietChild(t, pool, root, child, rank, phase.Admitted, "todo")
		seedMirrorChild(t, pool, child, "todo", root, blocker)
		seedMirror(t, pool, blocker, "in_progress")
	}

	const control, controlChild = "LEGION-600", "LEGION-601"
	seedQuietRoot(t, pool, control, "A", phase.AwaitingPerson)
	seedGate(t, pool, record.DesignGate{Issue: control, ArtifactID: "artifact-" + control, LatestVersion: 1,
		ApprovedVersion: &approved, EverOpened: true})
	seedMirror(t, pool, control, "in_progress")
	seedClaim(t, records, control, control, claim.RoleArchitect, supervise.StateIdle)
	blockedChild(control, controlChild, "A1", "LEGION-609")
	const heldParent, heldChild = "LEGION-610", "LEGION-611"
	treeOf(heldParent, "B", &record.Hold{From: phase.Retro, Reason: record.HoldForChildren})
	blockedChild(heldParent, heldChild, "B1", "LEGION-619")
	const stopped = "LEGION-620"
	treeOf(stopped, "C", &record.Hold{From: phase.Merging, Reason: record.HoldTaskStopped})

	// The premise, read from the one walk the measure reads (record.Waiting's second answer): each
	// held root reads the word of its hold, the held parent's child the Dispatch blocker that
	// holds it, and the control's child its root's wait on a person, each a wait by design.
	facts, slots := passFacts(t, pool)
	words := passWords(t, pool, facts, slots)
	for key, want := range map[string]record.HeldReason{
		heldParent: record.HeldChildrenBeforeRetro, stopped: record.HeldTaskStopped,
		heldChild: record.HeldDispatchBlocker, controlChild: record.HeldRootAwaitingPerson,
	} {
		if got := words[key]; got != want {
			t.Fatalf("the line leaves %s behind for %q, want %q", key, got, want)
		}
	}

	settled := settleMovements(t, pool, control, controlChild, heldParent, heldChild, stopped)
	admission.now = func() time.Time { return settled.Add(bound + time.Hour) }
	tick(t, pool, admission, "holds of a plan")

	if got, want := stalledRoots(t, pool), []string{heldParent, stopped}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the controller was told of %v, want %v: a held parent and a stopped task are no wait by design, and a tree whose root waits on a person is", got, want)
	}
}
